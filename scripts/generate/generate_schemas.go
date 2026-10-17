package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ── Configuration ──

const (
	cacheDir        = "../.cache/upstream-schemas" // relative to provider/
	fallbackVersion = "v0.0.0"                     // sentinel when latest detection fails
)

// ── Schema Cache ──
// Keyed by "provider@version" (e.g. "aws@v7.21.0"), holds parsed upstream schema.
var schemaCache = map[string]map[string]interface{}{}

// latestVersions tracks the latest detected version per upstream provider.
var latestVersions = map[string]string{}

// ── Component Discovery ──

type componentInfo struct {
	cloud      string // directory under provider/ (e.g. "aws", "gcp")
	folder     string // resource directory (e.g. "bucket", "function")
	schemaPath string
}

func discoverComponents(providerDir string) ([]componentInfo, error) {
	var components []componentInfo

	cloudDirs, err := os.ReadDir(providerDir)
	if err != nil {
		return nil, fmt.Errorf("reading provider directory %s: %w", providerDir, err)
	}

	skipDirs := map[string]bool{"cmd": true, "scripts": true, "sdk": true, "internal": true}

	for _, cloudEntry := range cloudDirs {
		if !cloudEntry.IsDir() || skipDirs[cloudEntry.Name()] {
			continue
		}
		cloud := cloudEntry.Name()
		cloudPath := filepath.Join(providerDir, cloud)

		resourceDirs, err := os.ReadDir(cloudPath)
		if err != nil {
			log.Printf("⚠️  Could not read %s: %v", cloudPath, err)
			continue
		}

		for _, resEntry := range resourceDirs {
			if !resEntry.IsDir() {
				continue
			}
			schemaPath := filepath.Join(cloudPath, resEntry.Name(), "schema.json")
			if _, err := os.Stat(schemaPath); err == nil {
				components = append(components, componentInfo{
					cloud:      cloud,
					folder:     resEntry.Name(),
					schemaPath: schemaPath,
				})
			}
		}
	}

	return components, nil
}

// ── Result Tracking ──

type componentResult struct {
	cloud       string
	folder      string
	provider    string
	pinned      string
	latest      string
	isStale     bool
	isMajorBump bool
	isNew       bool
	wasUpgraded bool
}

// ── Error Tracking ──

type componentError struct {
	cloud   string
	folder  string
	message string
}

func main() {
	// ── Parse flags ──
	upgradeMode := false
	clearCache := false
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--upgrade":
			upgradeMode = true
		case "--clear-cache":
			clearCache = true
		case "--help", "-h":
			fmt.Println("Usage: go run ../scripts/generate/generate_schemas.go [flags]")
			fmt.Println()
			fmt.Println("Flags:")
			fmt.Println("  --upgrade       Bump all components to the latest upstream provider version")
			fmt.Println("  --clear-cache   Delete cached upstream schemas before running")
			fmt.Println("  --help, -h      Show this help")
			os.Exit(0)
		default:
			log.Fatalf("❌ Unknown flag: %s (use --help for usage)", arg)
		}
	}

	if clearCache {
		fmt.Printf("🗑️  Clearing schema cache at %s\n", cacheDir)
		os.RemoveAll(cacheDir)
	}

	// ── Discover Components ──
	components, err := discoverComponents(".")
	if err != nil {
		log.Fatalf("❌ Failed to discover components: %v", err)
	}

	if len(components) == 0 {
		fmt.Println("No components found. Make sure provider/<cloud>/<resource>/schema.json files exist.")
		os.Exit(0)
	}

	fmt.Printf("🔍 Discovered %d component(s) across cloud providers\n\n", len(components))

	// ── Pre-scan: collect unique upstream providers for latest version detection ──
	upstreamProviders := map[string]bool{}
	for _, comp := range components {
		schemaBytes, err := os.ReadFile(comp.schemaPath)
		if err != nil {
			continue
		}
		var schema map[string]interface{}
		if err := json.Unmarshal(schemaBytes, &schema); err != nil {
			continue
		}
		if provider, ok := schema["x-upstream-provider"].(string); ok && provider != "" {
			upstreamProviders[provider] = true
		}
		// Backward compat: detect unmigrated AWS schemas
		if _, ok := schema["x-aws-token"].(string); ok {
			upstreamProviders["aws"] = true
		}
	}

	fmt.Println("⏳ Detecting latest versions for upstream providers...")
	for provider := range upstreamProviders {
		version := detectLatestVersion(provider)
		latestVersions[provider] = version
		fmt.Printf("   %s: %s\n", provider, version)
	}
	fmt.Println()

	if upgradeMode {
		fmt.Println("🔼 Upgrade mode: all components will be bumped to latest version")
		fmt.Println()
	}

	// ── Process Components ──
	startTime := time.Now()
	var results []componentResult
	var fatalErrors []componentError

	for _, comp := range components {
		schemaBytes, err := os.ReadFile(comp.schemaPath)
		if err != nil {
			continue
		}

		var anvilSchema map[string]interface{}
		if err := json.Unmarshal(schemaBytes, &anvilSchema); err != nil {
			log.Printf("⚠️  Error parsing %s: %v", comp.schemaPath, err)
			continue
		}

		// ── Backward Compatibility: migrate legacy fields ──
		migrateLegacyFields(anvilSchema, comp)

		upstreamProvider, _ := anvilSchema["x-upstream-provider"].(string)
		upstreamToken, _ := anvilSchema["x-upstream-token"].(string)

		if upstreamProvider == "" || upstreamToken == "" {
			fmt.Printf("⏭️  Skipping %s/%s: Missing x-upstream-provider or x-upstream-token\n", comp.cloud, comp.folder)
			continue
		}

		// ── Version Resolution ──
		latest, hasLatest := latestVersions[upstreamProvider]
		if !hasLatest {
			latest = detectLatestVersion(upstreamProvider)
			latestVersions[upstreamProvider] = latest
		}

		pinnedVersion, hasPinnedVersion := anvilSchema["x-upstream-version"].(string)
		isNewPin := false
		wasUpgraded := false

		if !hasPinnedVersion || pinnedVersion == "" {
			pinnedVersion = latest
			anvilSchema["x-upstream-version"] = pinnedVersion
			isNewPin = true
			fmt.Printf("📌 %s/%s: No x-upstream-version found — pinning to latest (%s)\n", comp.cloud, comp.folder, pinnedVersion)
		} else if upgradeMode && pinnedVersion != latest {
			fmt.Printf("🔼 %s/%s: Upgrading %s → %s\n", comp.cloud, comp.folder, pinnedVersion, latest)
			pinnedVersion = latest
			anvilSchema["x-upstream-version"] = pinnedVersion
			wasUpgraded = true
		}

		isStale := pinnedVersion != latest
		isMajorBump := isStale && isMajorVersionChange(pinnedVersion, latest)

		if isStale && !upgradeMode {
			if isMajorBump {
				fmt.Printf("⚠️  %s/%s: Pinned to %s (latest is %s) — MAJOR version change.\n", comp.cloud, comp.folder, pinnedVersion, latest)
				fmt.Printf("   Run with --upgrade or manually set \"x-upstream-version\": \"%s\" to upgrade.\n", latest)
			} else {
				fmt.Printf("ℹ️  %s/%s: Pinned to %s (latest is %s)\n", comp.cloud, comp.folder, pinnedVersion, latest)
			}
		}

		results = append(results, componentResult{
			cloud:       comp.cloud,
			folder:      comp.folder,
			provider:    upstreamProvider,
			pinned:      pinnedVersion,
			latest:      latest,
			isStale:     isStale,
			isMajorBump: isMajorBump,
			isNew:       isNewPin,
			wasUpgraded: wasUpgraded,
		})

		// ── Fetch upstream schema (trimmed) ──
		requiredTokens := []string{upstreamToken}
		upstreamSchema, err := getUpstreamSchema(upstreamProvider, pinnedVersion, requiredTokens)
		if err != nil {
			fatalErrors = append(fatalErrors, componentError{
				cloud:   comp.cloud,
				folder:  comp.folder,
				message: fmt.Sprintf("failed to fetch %s schema for %s: %v", upstreamProvider, pinnedVersion, err),
			})
			continue
		}

		upstreamResources, ok := upstreamSchema["resources"].(map[string]interface{})
		if !ok {
			fatalErrors = append(fatalErrors, componentError{
				cloud:   comp.cloud,
				folder:  comp.folder,
				message: fmt.Sprintf("invalid upstream schema format for %s@%s: missing 'resources'", upstreamProvider, pinnedVersion),
			})
			continue
		}

		fmt.Printf("⚙️  Processing: %s/%s → %s (provider: %s@%s)\n", comp.cloud, comp.folder, upstreamToken, upstreamProvider, pinnedVersion)

		mainResRaw, exists := upstreamResources[upstreamToken]
		if !exists {
			fatalErrors = append(fatalErrors, componentError{
				cloud:   comp.cloud,
				folder:  comp.folder,
				message: fmt.Sprintf("upstream resource \"%s\" not found in %s@%s", upstreamToken, upstreamProvider, pinnedVersion),
			})
			continue
		}
		mainRes := mainResRaw.(map[string]interface{})
		mainInputsRaw, _ := mainRes["inputProperties"].(map[string]interface{})

		if anvilSchema["types"] == nil {
			anvilSchema["types"] = make(map[string]interface{})
		}
		anvilTypes := anvilSchema["types"].(map[string]interface{})
		anvilResourceName := toPascalCase(comp.folder)

		// Anvil namespace: "anvil:aws:", "anvil:gcp:", etc.
		anvilNs := fmt.Sprintf("anvil:%s:", comp.cloud)

		cleanInputs := make(map[string]interface{})
		extraHatches := make(map[string]map[string]interface{})
		hatchArgsProps := make(map[string]interface{})

		// ── Generic Deprecation Replacement ──
		deprecationRegex := buildDeprecationRegex(upstreamProvider)

		for k, v := range mainInputsRaw {
			prop, ok := v.(map[string]interface{})
			if !ok {
				cleanInputs[k] = v
				continue
			}

			depMsg, hasDep := prop["deprecationMessage"].(string)
			if !hasDep || depMsg == "" {
				cleanInputs[k] = v
				continue
			}

			replacementToken := findReplacementToken(depMsg, deprecationRegex, upstreamProvider, upstreamResources)
			if replacementToken == "" {
				continue
			}

			if sidecarRaw, ok := upstreamResources[replacementToken]; ok {
				sidecar := sidecarRaw.(map[string]interface{})
				if sidecarInputs, ok := sidecar["inputProperties"].(map[string]interface{}); ok {
					rewriteRefs(sidecarInputs, upstreamProvider, pinnedVersion)

					replacementName := extractResourceName(replacementToken)
					typeName := fmt.Sprintf("%s%sHatch", anvilNs, replacementName)

					extraHatches[k] = map[string]interface{}{
						"typeName":   typeName,
						"properties": sidecarInputs,
					}
				}
			}
		}

		// ── Provider-Specific Extras ──
		addProviderSpecificHatches(comp.cloud, upstreamToken, upstreamResources, anvilTypes, anvilNs, hatchArgsProps, upstreamProvider, pinnedVersion)

		// ── Final Wiring ──
		rewriteRefs(cleanInputs, upstreamProvider, pinnedVersion)

		hatchTypeName := fmt.Sprintf("%s%sOverrides", anvilNs, anvilResourceName)
		anvilTypes[hatchTypeName] = map[string]interface{}{"type": "object", "properties": cleanInputs}
		hatchArgsProps[comp.folder] = map[string]interface{}{"$ref": fmt.Sprintf("#/types/%s", hatchTypeName)}

		for key, data := range extraHatches {
			typeName := data["typeName"].(string)
			anvilTypes[typeName] = map[string]interface{}{"type": "object", "properties": data["properties"]}
			hatchArgsProps[key] = map[string]interface{}{"$ref": fmt.Sprintf("#/types/%s", typeName)}
		}

		hatchArgsTypeName := fmt.Sprintf("%s%sHatchArgs", anvilNs, anvilResourceName)
		anvilTypes[hatchArgsTypeName] = map[string]interface{}{"type": "object", "properties": hatchArgsProps}

		if resources, ok := anvilSchema["resources"].(map[string]interface{}); ok {
			var resKey string

			if resources, ok := anvilSchema["resources"].(map[string]interface{}); ok {
				for k := range resources {
					resKey = k
					break
				}
			}

			if resObj, ok := resources[resKey].(map[string]interface{}); ok {
				if inputs, ok := resObj["inputProperties"].(map[string]interface{}); ok {
					inputs["hatch"] = map[string]interface{}{"$ref": fmt.Sprintf("#/types/%s", hatchArgsTypeName)}
				}
			}
		}

		staleKey := fmt.Sprintf("%sHatchArgs", anvilNs)
		if staleKey != hatchArgsTypeName {
			delete(anvilTypes, staleKey)
		}

		finalJson, _ := json.MarshalIndent(anvilSchema, "", "  ")
		os.WriteFile(comp.schemaPath, finalJson, 0644)
		fmt.Printf("   ✅ Success! Mapped %d additional hatch types.\n\n", len(extraHatches))
	}

	// ── Final Summary ──
	fmt.Println("────────────────────────────────────────")
	fmt.Println("📋 Version Summary:")
	fmt.Println("────────────────────────────────────────")

	staleCount := 0
	majorCount := 0
	for _, r := range results {
		label := fmt.Sprintf("%s/%s", r.cloud, r.folder)
		status := "✅ current"
		if r.wasUpgraded {
			status = fmt.Sprintf("🔼 upgraded to %s", r.pinned)
		} else if r.isNew {
			status = fmt.Sprintf("📌 auto-pinned to %s", r.pinned)
		} else if r.isMajorBump {
			status = fmt.Sprintf("⚠️  MAJOR update available (%s → %s)", r.pinned, r.latest)
			staleCount++
			majorCount++
		} else if r.isStale {
			status = fmt.Sprintf("ℹ️  %s (latest: %s)", r.pinned, r.latest)
			staleCount++
		}
		fmt.Printf("  %-30s [%s] %s\n", label, r.provider, status)
	}

	if majorCount > 0 {
		fmt.Printf("\n⚠️  %d component(s) behind by a MAJOR version. Review breaking changes before running --upgrade.\n", majorCount)
	} else if staleCount > 0 {
		fmt.Printf("\nℹ️  %d component(s) behind by minor/patch versions. Run with --upgrade to bump all to latest.\n", staleCount)
	}

	// ── Error Summary ──
	if len(fatalErrors) > 0 {
		fmt.Println("\n────────────────────────────────────────")
		fmt.Println("❌ Errors:")
		fmt.Println("────────────────────────────────────────")
		for _, e := range fatalErrors {
			fmt.Printf("  %s/%s: %s\n", e.cloud, e.folder, e.message)
		}
		fmt.Printf("\n%d component(s) failed. Fix the errors above and re-run.\n", len(fatalErrors))
	}

	// ── Auto-Prune Cache ──
	pruneCache(results)

	fmt.Printf("\n✨ Total processing finished in: %v\n", time.Since(startTime))

	// ── Exit non-zero if any fatal errors occurred ──
	if len(fatalErrors) > 0 {
		os.Exit(1)
	}
}

// ── Legacy Migration ──

// migrateLegacyFields converts old x-aws-token / x-aws-version fields to
// x-upstream-provider / x-upstream-token / x-upstream-version.
func migrateLegacyFields(schema map[string]interface{}, comp componentInfo) {
	// Migrate x-aws-version → x-upstream-version
	if _, hasNew := schema["x-upstream-version"].(string); !hasNew {
		if legacyVersion, ok := schema["x-aws-version"].(string); ok && legacyVersion != "" {
			schema["x-upstream-version"] = legacyVersion
			delete(schema, "x-aws-version")
			fmt.Printf("🔄 %s/%s: Migrated x-aws-version → x-upstream-version\n", comp.cloud, comp.folder)
		}
	}

	// Migrate x-aws-token → x-upstream-provider + x-upstream-token
	if legacyToken, ok := schema["x-aws-token"].(string); ok && legacyToken != "" {
		if _, hasProvider := schema["x-upstream-provider"].(string); !hasProvider {
			schema["x-upstream-provider"] = "aws"
		}
		if _, hasToken := schema["x-upstream-token"].(string); !hasToken {
			schema["x-upstream-token"] = legacyToken
		}
		delete(schema, "x-aws-token")
		fmt.Printf("🔄 %s/%s: Migrated x-aws-token → x-upstream-provider + x-upstream-token\n", comp.cloud, comp.folder)
	}
}

// ── Cache Pruning ──

// pruneCache deletes any trimmed cache files that don't correspond to a
// provider@version currently used by any component.
func pruneCache(results []componentResult) {
	activeFiles := make(map[string]bool)
	for _, r := range results {
		activeFiles[fmt.Sprintf("%s-%s-trimmed.json", r.provider, r.pinned)] = true
		activeFiles[fmt.Sprintf("%s-latest-version.txt", r.provider)] = true
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}

	pruned := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !activeFiles[entry.Name()] {
			os.Remove(filepath.Join(cacheDir, entry.Name()))
			pruned++
		}
	}

	if pruned > 0 {
		fmt.Printf("\n🧹 Pruned %d stale cache file(s)\n", pruned)
	}
}

// ── Provider-Specific Extras ──

// addProviderSpecificHatches handles companion resources that can't be discovered
// through the generic deprecation replacement logic.
func addProviderSpecificHatches(
	cloud string,
	upstreamToken string,
	upstreamResources map[string]interface{},
	anvilTypes map[string]interface{},
	anvilNs string,
	hatchArgsProps map[string]interface{},
	upstreamProvider string,
	pinnedVersion string,
) {
	switch {
	// AWS S3 Bucket: PublicAccessBlock is a companion resource, not discovered via deprecation
	case cloud == "aws" && upstreamToken == "aws:s3/bucket:Bucket":
		pabToken := "aws:s3/bucketPublicAccessBlock:BucketPublicAccessBlock"
		if pabRaw, ok := upstreamResources[pabToken]; ok {
			pab := pabRaw.(map[string]interface{})
			if pabInputs, ok := pab["inputProperties"].(map[string]interface{}); ok {
				rewriteRefs(pabInputs, upstreamProvider, pinnedVersion)
				typeName := fmt.Sprintf("%sPABHatch", anvilNs)
				anvilTypes[typeName] = map[string]interface{}{"type": "object", "properties": pabInputs}
				hatchArgsProps["publicAccessBlock"] = map[string]interface{}{"$ref": fmt.Sprintf("#/types/%s", typeName)}
			}
		}

		// Add more provider-specific cases here as needed:
		// case cloud == "gcp" && upstreamToken == "gcp:...":
	}
}

// ── Deprecation Replacement Logic ──

func buildDeprecationRegex(upstreamProvider string) *regexp.Regexp {
	parts := strings.SplitN(upstreamProvider, "-", 2)
	providerShort := parts[0]
	pattern := fmt.Sprintf(`%s\.([a-zA-Z0-9]+)\.([a-zA-Z0-9]+)`, regexp.QuoteMeta(providerShort))
	return regexp.MustCompile(pattern)
}

func findReplacementToken(depMsg string, regex *regexp.Regexp, upstreamProvider string, upstreamResources map[string]interface{}) string {
	matches := regex.FindAllStringSubmatch(depMsg, -1)
	for _, match := range matches {
		if len(match) < 3 {
			continue
		}
		service := match[1]
		resourceName := match[2]

		candidates := buildTokenCandidates(upstreamProvider, service, resourceName)
		for _, candidate := range candidates {
			if _, exists := upstreamResources[candidate]; exists {
				return candidate
			}
		}
	}
	return ""
}

func buildTokenCandidates(provider, service, resourceName string) []string {
	lowerFirst := strings.ToLower(resourceName[:1]) + resourceName[1:]
	lowerService := strings.ToLower(service)

	return []string{
		fmt.Sprintf("%s:%s/%s:%s", provider, lowerService, lowerFirst, resourceName),
		fmt.Sprintf("%s:%s:%s", provider, lowerService, resourceName),
		fmt.Sprintf("%s:%s/%s:%s", provider, service, lowerFirst, resourceName),
		fmt.Sprintf("%s:%s:%s", provider, service, resourceName),
	}
}

func extractResourceName(token string) string {
	parts := strings.SplitN(token, ":", 3)
	if len(parts) >= 3 {
		return parts[2]
	}
	segments := strings.Split(token, "/")
	return segments[len(segments)-1]
}

// ── Upstream Schema Fetching & Trimmed Caching ──

type trimmedCache struct {
	Version   string                 `json:"version"`
	Provider  string                 `json:"provider"`
	Tokens    []string               `json:"tokens"`
	Resources map[string]interface{} `json:"resources"`
	Types     map[string]interface{} `json:"types"`
}

func detectLatestVersion(provider string) string {
	os.MkdirAll(cacheDir, 0755)
	versionFile := filepath.Join(cacheDir, fmt.Sprintf("%s-latest-version.txt", provider))

	if info, err := os.Stat(versionFile); err == nil {
		if time.Since(info.ModTime()) < 24*time.Hour {
			if v, err := os.ReadFile(versionFile); err == nil {
				version := strings.TrimSpace(string(v))
				if version != "" {
					fmt.Printf("   (using cached latest version for %s)\n", provider)
					return version
				}
			}
		}
	}

	// Fetch full schema into memory just to read the version — never written to disk
	schema, err := fetchFullSchema(provider, "")
	if err != nil {
		log.Printf("⚠️  Could not detect latest version for %s: %v", provider, err)
		return fallbackVersion
	}

	version, ok := schema["version"].(string)
	if !ok || version == "" {
		log.Printf("⚠️  %s schema has no version field", provider)
		return fallbackVersion
	}

	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}

	os.WriteFile(versionFile, []byte(version), 0644)

	// Keep in memory so we can trim from it later without re-fetching
	cacheKey := fmt.Sprintf("%s@%s", provider, version)
	schemaCache[cacheKey] = schema

	return version
}

func fetchFullSchema(provider, version string) (map[string]interface{}, error) {
	var arg string
	if version == "" {
		arg = provider
	} else {
		arg = fmt.Sprintf("%s@%s", provider, strings.TrimPrefix(version, "v"))
	}

	fmt.Printf("   ⏳ Fetching %s schema (this may take a moment)...\n", arg)

	cmd := exec.Command("pulumi", "package", "get-schema", arg)
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pulumi package get-schema %s failed: %v\n   stderr: %s", arg, err, stderr.String())
	}

	var schema map[string]interface{}
	if err := json.Unmarshal(out.Bytes(), &schema); err != nil {
		return nil, fmt.Errorf("failed to parse schema for %s: %v", arg, err)
	}

	return schema, nil
}

// getUpstreamSchema returns the upstream schema containing at least the given resource tokens.
// Checks: memory cache → disk cache (trimmed) → full fetch + trim + cache.
func getUpstreamSchema(provider, version string, requiredTokens []string) (map[string]interface{}, error) {
	cacheKey := fmt.Sprintf("%s@%s", provider, version)

	// 1. Memory cache
	if schema, ok := schemaCache[cacheKey]; ok {
		if resources, ok := schema["resources"].(map[string]interface{}); ok {
			if hasAllTokens(resources, requiredTokens) {
				return schema, nil
			}
		}
		fmt.Printf("   📦 Memory cache for %s@%s missing tokens, re-fetching\n", provider, version)
		delete(schemaCache, cacheKey)
	}

	// 2. Disk cache (trimmed)
	os.MkdirAll(cacheDir, 0755)
	cachePath := filepath.Join(cacheDir, fmt.Sprintf("%s-%s-trimmed.json", provider, version))

	if data, err := os.ReadFile(cachePath); err == nil {
		var cached trimmedCache
		if err := json.Unmarshal(data, &cached); err == nil {
			if hasAllTokens(cached.Resources, requiredTokens) {
				schema := map[string]interface{}{
					"resources": cached.Resources,
					"types":     cached.Types,
					"version":   cached.Version,
				}
				schemaCache[cacheKey] = schema
				sizeKB := len(data) / 1024
				fmt.Printf("   📦 Loaded trimmed %s@%s from cache (%dKB, %d resource(s))\n",
					provider, version, sizeKB, len(cached.Tokens))
				return schema, nil
			}
			fmt.Printf("   📦 Trimmed cache for %s@%s missing tokens, re-fetching\n", provider, version)
		}
	}

	// 3. Fetch full schema into memory (never to disk)
	fullSchema, err := fetchFullSchema(provider, version)
	if err != nil {
		return nil, err
	}

	fetchedVersion, _ := fullSchema["version"].(string)
	if fetchedVersion != "" {
		normalized := fetchedVersion
		if !strings.HasPrefix(normalized, "v") {
			normalized = "v" + normalized
		}
		if normalized != version {
			fmt.Printf("   ⚠️  Requested %s@%s but got %s\n", provider, version, normalized)
		}
	}

	allResources, _ := fullSchema["resources"].(map[string]interface{})
	allTypes, _ := fullSchema["types"].(map[string]interface{})
	if allTypes == nil {
		allTypes = map[string]interface{}{}
	}

	// 4. Expand tokens: discover sidecar tokens from deprecation messages
	expandedTokens := expandTokensWithSidecars(requiredTokens, allResources, provider)

	// 5. Trim: start with any existing cached data and add the new tokens on top
	trimmedResources := make(map[string]interface{})
	trimmedTypes := make(map[string]interface{})
	var existingTokens []string

	// Load existing trimmed cache to merge into (if any)
	if data, err := os.ReadFile(cachePath); err == nil {
		var existing trimmedCache
		if err := json.Unmarshal(data, &existing); err == nil {
			for k, v := range existing.Resources {
				trimmedResources[k] = v
			}
			for k, v := range existing.Types {
				trimmedTypes[k] = v
			}
			existingTokens = existing.Tokens
		}
	}

	// Add newly required resources and their referenced types
	for _, token := range expandedTokens {
		if res, ok := allResources[token]; ok {
			trimmedResources[token] = res
			if resMap, ok := res.(map[string]interface{}); ok {
				if inputs, ok := resMap["inputProperties"].(map[string]interface{}); ok {
					collectReferencedTypes(inputs, allTypes, trimmedTypes, provider)
				}
			}
		}
	}

	// Merge token lists (deduplicated)
	tokenSet := make(map[string]bool)
	for _, t := range existingTokens {
		tokenSet[t] = true
	}
	for _, t := range expandedTokens {
		tokenSet[t] = true
	}
	allTokens := make([]string, 0, len(tokenSet))
	for t := range tokenSet {
		allTokens = append(allTokens, t)
	}

	schema := map[string]interface{}{
		"resources": trimmedResources,
		"types":     trimmedTypes,
		"version":   fetchedVersion,
	}

	// Write merged trimmed cache to disk
	cached := trimmedCache{
		Version:   version,
		Provider:  provider,
		Tokens:    allTokens,
		Resources: trimmedResources,
		Types:     trimmedTypes,
	}
	if cacheData, err := json.Marshal(cached); err == nil {
		os.WriteFile(cachePath, cacheData, 0644)
		sizeKB := len(cacheData) / 1024
		fmt.Printf("   ✅ %s@%s trimmed and cached (%dKB, %d resource(s), %d type(s))\n",
			provider, version, sizeKB, len(trimmedResources), len(trimmedTypes))
	}

	schemaCache[cacheKey] = schema
	return schema, nil
}

func hasAllTokens(resources map[string]interface{}, tokens []string) bool {
	for _, token := range tokens {
		if _, ok := resources[token]; !ok {
			return false
		}
	}
	return true
}

func expandTokensWithSidecars(tokens []string, allResources map[string]interface{}, provider string) []string {
	seen := make(map[string]bool)
	for _, t := range tokens {
		seen[t] = true
	}

	deprecationRegex := buildDeprecationRegex(provider)

	for _, token := range tokens {
		resRaw, ok := allResources[token]
		if !ok {
			continue
		}
		res, ok := resRaw.(map[string]interface{})
		if !ok {
			continue
		}
		inputs, ok := res["inputProperties"].(map[string]interface{})
		if !ok {
			continue
		}

		for _, v := range inputs {
			prop, ok := v.(map[string]interface{})
			if !ok {
				continue
			}
			depMsg, hasDep := prop["deprecationMessage"].(string)
			if !hasDep || depMsg == "" {
				continue
			}
			replacement := findReplacementToken(depMsg, deprecationRegex, provider, allResources)
			if replacement != "" && !seen[replacement] {
				seen[replacement] = true
			}
		}
	}

	// Also add hardcoded companion tokens
	for _, token := range tokens {
		for _, companion := range getCompanionTokens(token) {
			if !seen[companion] {
				seen[companion] = true
			}
		}
	}

	result := make([]string, 0, len(seen))
	for t := range seen {
		result = append(result, t)
	}
	return result
}

// getCompanionTokens returns any hardcoded companion resource tokens for a given primary token.
// This is the counterpart to addProviderSpecificHatches — ensures companions are included in the trimmed cache.
func getCompanionTokens(primaryToken string) []string {
	switch primaryToken {
	case "aws:s3/bucket:Bucket":
		return []string{"aws:s3/bucketPublicAccessBlock:BucketPublicAccessBlock"}
	default:
		return nil
	}
}

// collectReferencedTypes recursively walks an object tree, finds $ref pointers to upstream types,
// and copies those types (and their transitive dependencies) into the trimmed types map.
func collectReferencedTypes(obj interface{}, allTypes, trimmedTypes map[string]interface{}, provider string) {
	refPrefix := fmt.Sprintf("#/types/%s:", provider)

	switch v := obj.(type) {
	case map[string]interface{}:
		for key, val := range v {
			if key == "$ref" {
				if strVal, ok := val.(string); ok && strings.HasPrefix(strVal, refPrefix) {
					typeToken := strings.TrimPrefix(strVal, "#/types/")
					if _, alreadyCopied := trimmedTypes[typeToken]; !alreadyCopied {
						if typeDef, exists := allTypes[typeToken]; exists {
							trimmedTypes[typeToken] = typeDef
							collectReferencedTypes(typeDef, allTypes, trimmedTypes, provider)
						}
					}
				}
			} else {
				collectReferencedTypes(val, allTypes, trimmedTypes, provider)
			}
		}
	case []interface{}:
		for _, val := range v {
			collectReferencedTypes(val, allTypes, trimmedTypes, provider)
		}
	}
}

// ── Helpers ──

func parseMajorVersion(version string) int {
	v := strings.TrimPrefix(version, "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) == 0 {
		return -1
	}
	var major int
	if _, err := fmt.Sscanf(parts[0], "%d", &major); err != nil {
		return -1
	}
	return major
}

func isMajorVersionChange(pinned, latest string) bool {
	pinnedMajor := parseMajorVersion(pinned)
	latestMajor := parseMajorVersion(latest)
	if pinnedMajor == -1 || latestMajor == -1 {
		return true
	}
	return pinnedMajor != latestMajor
}

func toPascalCase(s string) string {
	parts := regexp.MustCompile(`[-_]`).Split(s, -1)
	var result string
	for _, p := range parts {
		if len(p) > 0 {
			result += strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return result
}

func rewriteRefs(obj interface{}, provider string, version string) {
	refPrefix := fmt.Sprintf("#/types/%s:", provider)

	switch v := obj.(type) {
	case map[string]interface{}:
		for key, val := range v {
			if key == "$ref" {
				if strVal, ok := val.(string); ok && strings.HasPrefix(strVal, refPrefix) {
					v[key] = fmt.Sprintf("/%s/%s/schema.json%s", provider, version, strVal)
				}
			} else {
				rewriteRefs(val, provider, version)
			}
		}
	case []interface{}:
		for _, val := range v {
			rewriteRefs(val, provider, version)
		}
	}
}
