package cmd

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/spf13/cobra"

	"github.com/anvil-cloud/anvil/cmd/anvil/compliance"
)

var (
	dashboardPort   int
	dashboardNoOpen bool
)

var complianceDashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Open a local dashboard of this app's compliance results",
	Long: `Starts a dashboard on 127.0.0.1 showing this app's scan results, grouped by
Anvil component. Results are read from your account with your AWS credentials
and kept in memory; nothing is written to disk or sent anywhere else.`,
	RunE: runComplianceDashboard,
}

func init() {
	complianceDashboardCmd.Flags().StringVar(&complianceStage, "stage", "", "Stage (defaults to the active stage)")
	complianceDashboardCmd.Flags().IntVar(&dashboardPort, "port", 0, "Port to listen on (default: a free port)")
	complianceDashboardCmd.Flags().BoolVar(&dashboardNoOpen, "no-open", false, "Print the URL instead of opening a browser")
	complianceCmd.AddCommand(complianceDashboardCmd)
}

func runComplianceDashboard(cmd *cobra.Command, args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	project, err := readProjectName()
	if err != nil {
		return err
	}
	shared, stage, err := loadShared(ctx)
	if err != nil {
		return err
	}
	st, err := shared.Status(ctx)
	if err != nil {
		return err
	}
	if !st.Exists {
		return fmt.Errorf("no compliance scanner in account %s (%s).\n  Add `compliance` to your App and deploy, or run `anvil compliance setup`.", shared.Names.Account, shared.Names.Region)
	}

	scans, err := shared.ListScans(ctx, project, stage, 1)
	if err != nil {
		return err
	}
	if len(scans) == 0 {
		return fmt.Errorf("%s/%s has no scans yet.\n  Run `anvil compliance scan --wait` first.", project, stage)
	}
	printCheck(fmt.Sprintf("Latest scan: %s", scans[0].ID))

	// Map resources to their Anvil components via the stack's Pulumi state.
	// Without it the dashboard still works, grouping by Anvil's Component tags.
	comps, lastDeploy, err := stackComponents(ctx, stage)
	if err != nil {
		printWarn(fmt.Sprintf("Couldn't read stack state, so resources aren't grouped by component: %v", err))
	} else {
		printCheck(fmt.Sprintf("Mapped %d resources to Anvil components", len(comps)))
	}

	srv := &dashboardServer{
		shared: shared, project: project, stage: stage,
		comps: comps, lastDeploy: lastDeploy, views: map[string]compliance.View{},
	}
	return srv.serve(ctx)
}

// stackComponents maps each resource ARN in the stage's state to the top-level
// Anvil component that created it, and returns the last deploy's end time.
func stackComponents(ctx context.Context, stage string) (map[string]compliance.Component, string, error) {
	s, err := loadStack(ctx, stage)
	if err != nil {
		return nil, "", err
	}
	exported, err := s.Export(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("exporting stack state: %w", err)
	}
	var dep apitype.DeploymentV3
	if err := json.Unmarshal(exported.Deployment, &dep); err != nil {
		return nil, "", fmt.Errorf("reading stack state: %w", err)
	}

	byURN := make(map[string]apitype.ResourceV3, len(dep.Resources))
	for _, r := range dep.Resources {
		byURN[string(r.URN)] = r
	}
	comps := map[string]compliance.Component{}
	for _, r := range dep.Resources {
		arn := outputString(r.Outputs, "arn")
		if arn == "" || !r.Custom {
			continue
		}
		if c, ok := topComponent(r, byURN); ok {
			comps[compliance.NormaliseARN(arn)] = c
		}
	}

	lastDeploy := ""
	if hist, err := s.History(ctx, 1, 1); err == nil && len(hist) > 0 && hist[0].EndTime != nil {
		lastDeploy = *hist[0].EndTime
	}
	return comps, lastDeploy, nil
}

// topComponent walks up the parent chain and returns the outermost Anvil
// component, e.g. the SvelteKitSite rather than a Lambda nested inside it.
func topComponent(r apitype.ResourceV3, byURN map[string]apitype.ResourceV3) (compliance.Component, bool) {
	var found compliance.Component
	ok := false
	for parent := string(r.Parent); parent != ""; {
		p, exists := byURN[parent]
		if !exists {
			break
		}
		t := string(p.Type)
		if !p.Custom && strings.HasPrefix(t, "anvil:") && t != "anvil:aws:ComplianceScanner" {
			found = compliance.Component{Name: p.URN.Name(), Type: t[strings.LastIndex(t, ":")+1:]}
			ok = true
		}
		parent = string(p.Parent)
	}
	return found, ok
}

// ── Server ──────────────────────────────────────────────────

type dashboardServer struct {
	shared     *compliance.Shared
	project    string
	stage      string
	comps      map[string]compliance.Component
	lastDeploy string

	token string
	host  string

	mu    sync.Mutex
	views map[string]compliance.View // by scan ID; stored scans never change
}

func (d *dashboardServer) serve(ctx context.Context) error {
	tok := make([]byte, 24)
	if _, err := rand.Read(tok); err != nil {
		return err
	}
	d.token = hex.EncodeToString(tok)

	// Loopback only: the dashboard is never reachable from the network.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", dashboardPort))
	if err != nil {
		return fmt.Errorf("starting dashboard: %w", err)
	}
	d.host = ln.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", d.index)
	mux.HandleFunc("GET /api/scans", d.api(d.scans))
	mux.HandleFunc("GET /api/scan", d.api(d.scan))
	mux.HandleFunc("POST /api/rescan", d.api(d.rescan))

	server := &http.Server{Handler: d.guard(mux), ReadHeaderTimeout: 10 * time.Second}
	url := fmt.Sprintf("http://%s/?token=%s#/overview", d.host, d.token)

	fmt.Println()
	fmt.Printf("  Dashboard running at %s\n", url)
	fmt.Println(dim("  Press Ctrl+C to stop."))
	if !dashboardNoOpen {
		if err := openBrowser(url); err != nil {
			printWarn("Couldn't open a browser; open the URL above.")
		}
	}

	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		fmt.Println("\n  Dashboard stopped.")
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// guard rejects requests for any other Host (DNS-rebinding protection) and
// sets strict headers on every response.
func (d *dashboardServer) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != d.host {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		// The token is in the URL the CLI opens: never send it as a referrer
		// when the user follows an external reference link.
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (d *dashboardServer) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(compliance.DashboardHTML)
}

// api checks the per-run token (a custom header, so a cross-site page can't
// send it without a CORS preflight, which is never granted) and writes JSON.
func (d *dashboardServer) api(fn func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Anvil-Token")), []byte(d.token)) != 1 {
			http.Error(w, "missing or invalid dashboard token — reopen the URL printed by the CLI", http.StatusUnauthorized)
			return
		}
		v, err := fn(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (d *dashboardServer) scans(r *http.Request) (any, error) {
	return d.shared.ListScans(r.Context(), d.project, d.stage, 50)
}

func (d *dashboardServer) scan(r *http.Request) (any, error) {
	ctx := r.Context()
	scans, err := d.shared.ListScans(ctx, d.project, d.stage, 50)
	if err != nil {
		return nil, err
	}
	if len(scans) == 0 {
		return nil, fmt.Errorf("no scans yet")
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		id = scans[0].ID
	}

	d.mu.Lock()
	cached, ok := d.views[id]
	d.mu.Unlock()
	if ok {
		return cached, nil
	}

	current, err := d.shared.LoadScan(ctx, d.project, d.stage, id)
	if err != nil {
		return nil, err
	}

	// "What changed" compares against the next older complete scan.
	var previous *compliance.ScanResult
	if current.Manifest.Status == "complete" {
		older := false
		for _, s := range scans {
			if s.ID == id {
				older = true
				continue
			}
			if !older {
				continue
			}
			if p, err := d.shared.LoadScan(ctx, d.project, d.stage, s.ID); err == nil && p.Manifest.Status == "complete" {
				previous = p
				break
			}
		}
	}

	view := compliance.BuildView(compliance.ViewInput{
		Project: d.project, Stage: d.stage, Account: d.shared.Names.Account, Region: d.shared.Names.Region,
		Current: current, Previous: previous, Components: d.comps, LastDeploy: d.lastDeploy,
	})
	d.mu.Lock()
	d.views[id] = view
	d.mu.Unlock()
	return view, nil
}

// rescan repeats the latest scan's settings with a manual trigger.
func (d *dashboardServer) rescan(r *http.Request) (any, error) {
	ctx := r.Context()
	m, err := d.shared.LatestManifest(ctx, d.project, d.stage)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("no previous scan to repeat; run `anvil compliance scan`")
	}
	retention := "1y"
	var full struct {
		Retention string `json:"retention"`
	}
	if res, err := d.shared.LoadScan(ctx, d.project, d.stage, m.ScanID); err == nil {
		if json.Unmarshal(res.Raw, &full) == nil && full.Retention != "" {
			retention = full.Retention
		}
	}
	buildID, err := d.shared.StartScan(ctx, compliance.ScanRequest{
		Project: d.project, Stage: d.stage, Regions: m.Regions,
		Frameworks: m.Frameworks, Retention: retention, Trigger: "manual",
	})
	if err != nil {
		return nil, err
	}
	printCheck("Scan started from the dashboard")
	return map[string]string{"buildId": buildID}, nil
}

func openBrowser(url string) error {
	switch goruntime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
