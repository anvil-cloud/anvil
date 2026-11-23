package awssite

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/cloudfront"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// Origin protection locks a site's CloudFront distribution to the CDN/proxy in
// front of it (Cloudflare, Fastly, Akamai, nginx, ...). The proxy adds the
// x-origin-secret header to every request; a CloudFront Function rejects
// requests without it and strips the header before the origin sees it.
// See docs/design/site-protection.md.

//go:embed edge/viewer-request.js
var viewerRequestCode string

// OriginSecretHeader is the header the proxy must send on every request.
const OriginSecretHeader = "x-origin-secret"

// originSecretKey is the KeyValueStore key holding the secret. Must match
// SECRET_KEYS in edge/origin-guard.js.
const originSecretKey = "origin-secret"

// OriginProtectionResult holds the outputs of SetupOriginProtection.
type OriginProtectionResult struct {
	// StoreArn is the KeyValueStore holding the secret; the viewer-request
	// function reads it.
	StoreArn pulumi.StringOutput

	// secretKey is the KeyValueStore entry; the function depends on it.
	secretKey pulumi.Resource

	// OriginSecret is the header value to configure in the proxy. Secret.
	OriginSecret pulumi.StringOutput
}

// SetupOriginProtection creates the secret the viewer-request function checks
// (see CreateViewerRequestFunction).
//
// The secret is stored in a CloudFront KeyValueStore rather than in the
// function code, so reading the function doesn't reveal it. It is generated
// on first deploy and kept stable afterwards: the key ignores changes to its
// value, so later deploys (which generate a fresh candidate) leave it alone
// and the proxy configuration keeps working.
func SetupOriginProtection(ctx *pulumi.Context, parent pulumi.Resource, name string) (*OriginProtectionResult, error) {
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return nil, fmt.Errorf("failed to generate origin secret: %w", err)
	}

	store := &cloudfront.KeyValueStore{}
	err := ctx.RegisterResource("aws:cloudfront/keyValueStore:KeyValueStore", name+"-origin-kvs", pulumi.Map{
		"comment": pulumi.Sprintf("Anvil origin protection secret for site %s", name),
	}, store, pulumi.Parent(parent))
	if err != nil {
		return nil, fmt.Errorf("failed to create origin secret store: %w", err)
	}

	secretKey := &cloudfront.KeyvaluestoreKey{}
	err = ctx.RegisterResource("aws:cloudfront/keyvaluestoreKey:KeyvaluestoreKey", name+"-origin-secret", pulumi.Map{
		"keyValueStoreArn": store.Arn,
		"key":              pulumi.String(originSecretKey),
		"value":            pulumi.ToSecret(pulumi.String(hex.EncodeToString(secretBytes))),
	}, secretKey,
		pulumi.Parent(parent),
		pulumi.IgnoreChanges([]string{"value"}),
		pulumi.AdditionalSecretOutputs([]string{"value"}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to store origin secret: %w", err)
	}

	return &OriginProtectionResult{
		StoreArn:     store.Arn,
		secretKey:    secretKey,
		OriginSecret: pulumi.ToSecret(secretKey.Value).(pulumi.StringOutput),
	}, nil
}

// SiteRedirect sends every request for From to the same path and query on To.
type SiteRedirect struct {
	From, To string
}

// CreateViewerRequestFunction creates the site's single viewer-request
// CloudFront Function: the origin guard (when guard is set), then the redirect
// (when redirect is set). Returns a zero output when neither is needed.
//
// The resource keeps the "-origin-guard" name whenever the guard is on, so
// adding or removing the redirect only changes the function's code.
func CreateViewerRequestFunction(ctx *pulumi.Context, parent pulumi.Resource, name string, guard *OriginProtectionResult, redirect *SiteRedirect) (pulumi.StringOutput, error) {
	if guard == nil && redirect == nil {
		return pulumi.StringOutput{}, nil
	}

	from, to := "", ""
	if redirect != nil {
		from, to = strings.ToLower(redirect.From), strings.ToLower(redirect.To)
	}
	render := func(kvsID string) string {
		return strings.NewReplacer(
			"__KVS_ID__", kvsID,
			"__REDIRECT_HOST__", from,
			"__CANONICAL_HOST__", to,
		).Replace(viewerRequestCode)
	}

	props := pulumi.Map{
		"runtime": pulumi.String("cloudfront-js-2.0"),
		"publish": pulumi.Bool(true),
	}
	resourceName := name + "-viewer-request"
	opts := []pulumi.ResourceOption{pulumi.Parent(parent)}

	if guard != nil {
		resourceName = name + "-origin-guard"
		// cf.kvs() takes the store ID, the last segment of its ARN.
		props["code"] = guard.StoreArn.ApplyT(func(arn string) string {
			return render(arn[strings.LastIndex(arn, "/")+1:])
		}).(pulumi.StringOutput)
		props["keyValueStoreAssociations"] = pulumi.StringArray{guard.StoreArn}
		props["comment"] = pulumi.Sprintf("Anvil origin protection for site %s", name)
		opts = append(opts, pulumi.DependsOn([]pulumi.Resource{guard.secretKey}))
	} else {
		props["code"] = pulumi.String(render(""))
		props["comment"] = pulumi.Sprintf("Anvil %s redirect for site %s", from, name)
	}

	fn := &cloudfront.Function{}
	if err := ctx.RegisterResource("aws:cloudfront/function:Function", resourceName, props, fn, opts...); err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("failed to create viewer-request function: %w", err)
	}
	return fn.Arn, nil
}
