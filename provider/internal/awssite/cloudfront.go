package awssite

import (
	"os"
	"regexp"
	"strings"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/cloudfront"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/s3"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// CloudFrontArgs holds the inputs for BuildCloudFrontArgs.
type CloudFrontArgs struct {
	// Name is the component name, used to derive origin IDs.
	Name string

	// Bucket is the S3 bucket for static assets (S3 origin).
	Bucket *s3.Bucket

	// OAC is the Origin Access Control for the S3 origin.
	OAC *cloudfront.OriginAccessControl

	// LambdaOriginDomain is the hostname of the Lambda Function URL (no scheme, no trailing slash).
	LambdaOriginDomain pulumi.StringOutput

	// LambdaOAC is the Origin Access Control for the Lambda origin. Only set when
	// the Function URL is IAM-protected; nil means the Lambda origin is unsigned.
	LambdaOAC *cloudfront.OriginAccessControl

	// Domain is the optional custom domain. Empty string means use the CloudFront default cert.
	Domain string

	// RedirectDomain is an extra alias that the viewer-request function
	// redirects to Domain (the apex/www pair). Empty means none.
	RedirectDomain string

	// CertARN is the ACM certificate ARN. Only required when Domain is set.
	CertARN pulumi.StringOutput

	// OrderedCacheBehaviors defines framework-specific path patterns routed to S3.
	// SvelteKit passes /_app/immutable/* and /_app/*.
	// Astro will pass /_astro/*.
	// The default cache behavior (all other paths) always routes to Lambda.
	OrderedCacheBehaviors pulumi.Array

	// WebACLArn is the ARN of the WAF WebACL to associate with the distribution.
	// Only set when the site's waf input is set. Zero value means no WAF.
	WebACLArn pulumi.StringOutput

	// ViewerRequestFunctionArn is a CloudFront Function run on viewer-request for
	// every cache behavior (origin protection and/or the apex<->www redirect).
	// Zero value means none.
	ViewerRequestFunctionArn pulumi.StringOutput

	// EdgeSignerArn is the qualified ARN of the Lambda@Edge signer run on
	// origin-request for the Lambda origin (edge-oac). Zero value means none.
	EdgeSignerArn pulumi.StringOutput

	// ResponseHeadersPolicyID is the security headers policy applied to every
	// cache behavior. Zero value means none.
	ResponseHeadersPolicyID pulumi.StringOutput
}

// BuildCloudFrontArgs constructs the full CloudFront distribution argument map.
// The default cache behavior routes to Lambda (SSR). Framework-specific static
// asset paths are routed to S3 via OrderedCacheBehaviors.
func BuildCloudFrontArgs(args CloudFrontArgs) pulumi.Map {
	s3OriginID := args.Name + "-s3"
	lambdaOriginID := args.Name + "-lambda"

	lambdaOrigin := pulumi.Map{
		"domainName": args.LambdaOriginDomain,
		"originId":   pulumi.String(lambdaOriginID),
		"customOriginConfig": pulumi.Map{
			"httpPort":             pulumi.Int(80),
			"httpsPort":            pulumi.Int(443),
			"originProtocolPolicy": pulumi.String("https-only"),
			"originSslProtocols":   pulumi.StringArray{pulumi.String("TLSv1.2")},
		},
	}
	// Sign origin requests so the AWS_IAM Function URL accepts them.
	if args.LambdaOAC != nil {
		lambdaOrigin["originAccessControlId"] = args.LambdaOAC.ID()
	}

	var viewerCertificate pulumi.Map
	if args.Domain != "" {
		viewerCertificate = pulumi.Map{
			"acmCertificateArn":      args.CertARN,
			"sslSupportMethod":       pulumi.String("sni-only"),
			"minimumProtocolVersion": pulumi.String("TLSv1.2_2021"),
		}
	} else {
		viewerCertificate = pulumi.Map{
			"cloudfrontDefaultCertificate": pulumi.Bool(true),
		}
	}

	cfArgs := pulumi.Map{
		"enabled":           pulumi.Bool(true),
		"isIpv6Enabled":     pulumi.Bool(true),
		"httpVersion":       pulumi.String("http2and3"),
		"priceClass":        pulumi.String("PriceClass_100"),
		"defaultRootObject": pulumi.String(""),
		"origins": pulumi.Array{
			pulumi.Map{
				"domainName":            args.Bucket.BucketRegionalDomainName,
				"originId":              pulumi.String(s3OriginID),
				"originAccessControlId": args.OAC.ID(),
			},
			lambdaOrigin,
		},
		// Default: all requests → Lambda (SSR).
		"defaultCacheBehavior": pulumi.Map{
			"targetOriginId":        pulumi.String(lambdaOriginID),
			"viewerProtocolPolicy":  pulumi.String("redirect-to-https"),
			"allowedMethods":        pulumi.StringArray{pulumi.String("GET"), pulumi.String("HEAD"), pulumi.String("OPTIONS"), pulumi.String("PUT"), pulumi.String("POST"), pulumi.String("PATCH"), pulumi.String("DELETE")},
			"cachedMethods":         pulumi.StringArray{pulumi.String("GET"), pulumi.String("HEAD")},
			"compress":              pulumi.Bool(true),
			"cachePolicyId":         pulumi.String("4135ea2d-6df8-44a3-9df3-4b5a84be39ad"), // CachingDisabled
			"originRequestPolicyId": pulumi.String("b689b0a8-53d0-40ab-baf2-68738e2966ac"), // AllViewerExceptHostHeader
		},
		// Framework-specific static asset paths → S3.
		"orderedCacheBehaviors": args.OrderedCacheBehaviors,
		"restrictions": pulumi.Map{
			"geoRestriction": pulumi.Map{"restrictionType": pulumi.String("none")},
		},
		"viewerCertificate": viewerCertificate,
		"tags":              pulumi.StringMap{"ManagedBy": pulumi.String("anvil")},
	}

	if args.Domain != "" {
		aliases := pulumi.StringArray{pulumi.String(args.Domain)}
		if args.RedirectDomain != "" {
			aliases = append(aliases, pulumi.String(args.RedirectDomain))
		}
		cfArgs["aliases"] = aliases
	}

	if args.WebACLArn != (pulumi.StringOutput{}) {
		cfArgs["webAclId"] = args.WebACLArn
	}

	// Security headers go on every behavior — static assets included.
	if args.ResponseHeadersPolicyID != (pulumi.StringOutput{}) {
		cfArgs["defaultCacheBehavior"].(pulumi.Map)["responseHeadersPolicyId"] = args.ResponseHeadersPolicyID
		for _, behavior := range args.OrderedCacheBehaviors {
			behavior.(pulumi.Map)["responseHeadersPolicyId"] = args.ResponseHeadersPolicyID
		}
	}

	// The edge signer only applies to the Lambda origin (the default behavior);
	// S3 behaviors serve static GETs. includeBody lets it hash the request body.
	if args.EdgeSignerArn != (pulumi.StringOutput{}) {
		cfArgs["defaultCacheBehavior"].(pulumi.Map)["lambdaFunctionAssociations"] = pulumi.Array{pulumi.Map{
			"eventType":   pulumi.String("origin-request"),
			"lambdaArn":   args.EdgeSignerArn,
			"includeBody": pulumi.Bool(true),
		}}
	}

	// The viewer-request function must run on every behavior — static asset
	// paths included — or those paths would bypass it.
	if args.ViewerRequestFunctionArn != (pulumi.StringOutput{}) {
		associations := pulumi.Array{pulumi.Map{
			"eventType":   pulumi.String("viewer-request"),
			"functionArn": args.ViewerRequestFunctionArn,
		}}
		cfArgs["defaultCacheBehavior"].(pulumi.Map)["functionAssociations"] = associations
		for _, behavior := range args.OrderedCacheBehaviors {
			behavior.(pulumi.Map)["functionAssociations"] = associations
		}
	}

	return cfArgs
}

// S3CacheBehavior returns a CloudFront ordered cache behavior that routes a path
// pattern to S3 with long-term caching (CachingOptimized policy).
// Use for content-hashed static asset paths like /_app/immutable/* or /_astro/*.
func S3CacheBehavior(pathPattern string, s3OriginID string) pulumi.Map {
	return pulumi.Map{
		"pathPattern":          pulumi.String(pathPattern),
		"targetOriginId":       pulumi.String(s3OriginID),
		"viewerProtocolPolicy": pulumi.String("redirect-to-https"),
		"allowedMethods":       pulumi.StringArray{pulumi.String("GET"), pulumi.String("HEAD")},
		"cachedMethods":        pulumi.StringArray{pulumi.String("GET"), pulumi.String("HEAD")},
		"compress":             pulumi.Bool(true),
		"cachePolicyId":        pulumi.String("658327ea-f89d-4fab-a63d-7e88639e58f6"), // CachingOptimized
	}
}

// LambdaOriginDomainFromURL strips the scheme and trailing slash from a Lambda
// Function URL to produce a bare hostname suitable for a CloudFront custom origin.
func LambdaOriginDomainFromURL(fnURL pulumi.StringOutput) pulumi.StringOutput {
	return fnURL.ApplyT(func(url string) string {
		return strings.TrimSuffix(strings.TrimPrefix(url, "https://"), "/")
	}).(pulumi.StringOutput)
}

// maxStaticRootBehaviors keeps the distribution well under CloudFront's default
// quota of 25 cache behaviors, leaving room for framework and hosting behaviors.
const maxStaticRootBehaviors = 20

// cloudFrontPathChars are the characters CloudFront accepts in a path pattern.
var cloudFrontPathChars = regexp.MustCompile(`^[A-Za-z0-9_.\-~$@:+&'"]+$`)

// StaticRootBehaviors routes each top-level entry of a framework's static output
// (e.g. SvelteKit's static/ folder: robots.txt, favicon.png, .well-known/) to S3.
// Without these, only the framework's own asset prefixes reach S3 and every
// other uploaded file falls through to the server, which returns 404.
//
// skip lists top-level names already covered by framework behaviors (e.g.
// "_app"). Precompressed .br/.gz copies are skipped when their source file
// exists. Entries beyond the cap, or with names CloudFront can't match, are
// returned in skipped and stay on the server.
func StaticRootBehaviors(staticDir string, skip []string, s3OriginID string) (behaviors pulumi.Array, skipped []string, err error) {
	entries, err := os.ReadDir(staticDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}

	skipSet := map[string]bool{}
	for _, s := range skip {
		skipSet[s] = true
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}

	for _, e := range entries {
		name := e.Name()
		if skipSet[name] {
			continue
		}
		if !e.IsDir() {
			if base, ok := strings.CutSuffix(name, ".br"); ok && names[base] {
				continue
			}
			if base, ok := strings.CutSuffix(name, ".gz"); ok && names[base] {
				continue
			}
		}
		if !cloudFrontPathChars.MatchString(name) || len(behaviors) >= maxStaticRootBehaviors {
			skipped = append(skipped, name)
			continue
		}
		pattern := "/" + name
		if e.IsDir() {
			pattern += "/*"
		}
		behaviors = append(behaviors, S3CacheBehavior(pattern, s3OriginID))
	}
	return behaviors, skipped, nil
}
