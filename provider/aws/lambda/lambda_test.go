package lambda

import (
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

type noopMocks struct{}

func (noopMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	return args.Name + "_id", args.Inputs, nil
}

func (noopMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return resource.PropertyMap{}, nil
}

// runCtx runs fn with a mocked Pulumi context, as a preview (dryRun) or a deploy.
func runCtx(t *testing.T, dryRun bool, fn func(ctx *pulumi.Context) error) error {
	t.Helper()
	return pulumi.RunErr(fn,
		pulumi.WithMocks("project", "stack", noopMocks{}),
		func(info *pulumi.RunInfo) { info.DryRun = dryRun },
	)
}

func TestValidateArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    LambdaArgs
		wantErr string
	}{
		{"rust with entry", LambdaArgs{Runtime: "rust", Entry: "functions/api"}, ""},
		{"rust without entry", LambdaArgs{Runtime: "rust"}, `runtime "rust" requires entry, e.g. entry: "functions/api"`},
		{"node without entry deploys a placeholder", LambdaArgs{Runtime: "nodejs24.x"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateArgs("api", tt.args)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestResolveRuntime(t *testing.T) {
	tests := []struct {
		name        string
		args        LambdaArgs
		wantRuntime string
		wantHandler string
	}{
		{"rust", LambdaArgs{Runtime: "rust"}, "provided.al2023", "bootstrap"},
		{"rust ignores a user handler", LambdaArgs{Runtime: "rust", Handler: "index.handler"}, "provided.al2023", "bootstrap"},
		{"node default handler", LambdaArgs{Runtime: "nodejs24.x"}, "nodejs24.x", "index.handler"},
		{"node explicit handler", LambdaArgs{Runtime: "nodejs22.x", Handler: "index.main"}, "nodejs22.x", "index.main"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime, handler := resolveRuntime(tt.args)
			if runtime != tt.wantRuntime || handler != tt.wantHandler {
				t.Errorf("got (%q, %q), want (%q, %q)", runtime, handler, tt.wantRuntime, tt.wantHandler)
			}
		})
	}
}

func TestCodeArchive(t *testing.T) {
	rust := LambdaArgs{Runtime: "rust", Entry: "functions/api"}
	node := LambdaArgs{Runtime: "nodejs24.x"}

	t.Run("built zip is used as-is", func(t *testing.T) {
		err := runCtx(t, false, func(ctx *pulumi.Context) error {
			code, err := codeArchive(ctx, "api", rust, ".anvil/build/api.zip")
			if err != nil {
				return err
			}
			if got := code.Path(); got != ".anvil/build/api.zip" {
				t.Errorf("path = %q", got)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("rust without a build fails a deploy", func(t *testing.T) {
		err := runCtx(t, false, func(ctx *pulumi.Context) error {
			_, err := codeArchive(ctx, "api", rust, "")
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "no build artifact") {
			t.Fatalf("error = %v, want a missing-artifact error", err)
		}
	})

	t.Run("rust without a build still previews", func(t *testing.T) {
		err := runCtx(t, true, func(ctx *pulumi.Context) error {
			code, err := codeArchive(ctx, "api", rust, "")
			if err != nil {
				return err
			}
			if _, ok := code.Assets()["bootstrap"]; !ok {
				t.Errorf("placeholder has no bootstrap entry: %v", code.Assets())
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("node without a build deploys the placeholder", func(t *testing.T) {
		err := runCtx(t, false, func(ctx *pulumi.Context) error {
			code, err := codeArchive(ctx, "api", node, "")
			if err != nil {
				return err
			}
			if _, ok := code.Assets()["index.js"]; !ok {
				t.Errorf("placeholder has no index.js entry: %v", code.Assets())
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}
