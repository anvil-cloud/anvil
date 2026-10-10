package httpapi

import (
	"fmt"

	"github.com/anvil-cloud/anvil/provider/internal/apigateway"
	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/apigatewayv2"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	awslambda "github.com/pulumi/pulumi-aws/sdk/v7/go/aws/lambda"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	c "github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// ── Consumer types ─────────────────────────────────────────────────────────

// HttpApiLambdaConsumer routes requests to a Lambda function.
type HttpApiLambdaConsumer struct {
	// Arn is the Lambda function ARN. Pass lambda.arn directly.
	Arn pulumi.StringInput `pulumi:"arn"`
}

// HttpApiSqsConsumer routes requests directly to an SQS queue.
// API Gateway sends the request body as a message — no Lambda needed.
// Always responds 202 Accepted immediately (async).
type HttpApiSqsConsumer struct {
	// Url is the SQS queue URL. Pass queue.url directly.
	Url pulumi.StringInput `pulumi:"url"`
	// Arn is the SQS queue ARN. Pass queue.arn directly.
	Arn pulumi.StringInput `pulumi:"arn"`
}

// HttpApiEventBridgeConsumer routes requests directly to an EventBridge bus.
// API Gateway puts the request body as an event — no Lambda needed.
// Always responds 200 immediately (async).
type HttpApiEventBridgeConsumer struct {
	// Name is the EventBridge bus name. Pass bus.name directly.
	Name pulumi.StringInput `pulumi:"name"`
}

// HttpApiStepFunctionsConsumer routes requests to a Step Functions state machine.
// Async by default (StartExecution). Set Sync: true for StartSyncExecution —
// the response contains the execution result but has a 29s API Gateway timeout.
type HttpApiStepFunctionsConsumer struct {
	// Arn is the state machine ARN. Pass stateMachine.arn directly.
	Arn pulumi.StringInput `pulumi:"arn"`
	// Sync waits for execution to complete before responding.
	// Default: false (async). Note: sync executions are subject to the
	// 29s API Gateway timeout — only use for fast state machines.
	Sync bool `pulumi:"sync,optional"`
}

// HttpApiHttpConsumer proxies requests to an external HTTP URL.
// Useful for gradual migration from legacy APIs or third-party integrations.
type HttpApiHttpConsumer struct {
	// Url is the external HTTP URL to proxy to.
	Url string `pulumi:"url"`
}

// HttpApiRouteConsumer defines the target that handles a route.
// Exactly one field must be set — Anvil validates this at deploy time.
type HttpApiRouteConsumer struct {
	Lambda        *HttpApiLambdaConsumer        `pulumi:"lambda,optional"`
	Sqs           *HttpApiSqsConsumer           `pulumi:"sqs,optional"`
	EventBridge   *HttpApiEventBridgeConsumer   `pulumi:"eventBridge,optional"`
	StepFunctions *HttpApiStepFunctionsConsumer `pulumi:"stepFunctions,optional"`
	Http          *HttpApiHttpConsumer          `pulumi:"http,optional"`
}

// consumerType returns a string identifying which consumer field is set.
func (c *HttpApiRouteConsumer) consumerType() (string, error) {
	count := 0
	t := ""
	if c.Lambda != nil {
		count++
		t = "lambda"
	}
	if c.Sqs != nil {
		count++
		t = "sqs"
	}
	if c.EventBridge != nil {
		count++
		t = "eventBridge"
	}
	if c.StepFunctions != nil {
		count++
		t = "stepFunctions"
	}
	if c.Http != nil {
		count++
		t = "http"
	}
	if count == 0 {
		return "", fmt.Errorf("route consumer: at least one target must be specified (lambda, sqs, eventBridge, stepFunctions, http)")
	}
	if count > 1 {
		return "", fmt.Errorf("route consumer: only one target can be specified per route, got %d", count)
	}
	return t, nil
}

// ── Route ──────────────────────────────────────────────────────────────────

// HttpApiRoute defines a single route on the HTTP API.
type HttpApiRoute struct {
	// Method is the HTTP method. One of GET, POST, PUT, PATCH, DELETE, ANY.
	Method string `pulumi:"method"`

	// Path is the route path. e.g. "/users" or "/users/{id}".
	// Use {param} for path parameters and {proxy+} for greedy paths.
	Path string `pulumi:"path"`

	// Consumer is the target that handles this route.
	// Exactly one consumer type must be set.
	Consumer HttpApiRouteConsumer `pulumi:"consumer"`

	// Throttling overrides the API-level throttling for this specific route.
	// Omit to inherit API-level throttling.
	Throttling *HttpApiThrottling `pulumi:"throttling,optional"`

	// SkipAuth explicitly opts this route out of the API-level defaultAuthorizer.
	// Use for health checks, webhook endpoints, or any route that handles its own
	// validation. Has no effect if defaultAuthorizerId is not set on the API.
	SkipAuth bool `pulumi:"skipAuth,optional"`

	// Scopes is the list of OAuth scopes required to access this route.
	// API Gateway rejects tokens that do not contain all listed scopes.
	// Only applies when defaultAuthorizerId is set and skipAuth is false.
	// Example: ["read:users", "write:users"]
	Scopes []string `pulumi:"scopes,optional"`
}

// ── Throttling ─────────────────────────────────────────────────────────────

// HttpApiThrottling configures rate and burst limits.
// Applied at the API level by default; can be overridden per route.
//
// Defaults: rateLimit 1000 rps, burstLimit 500 concurrent — the Serverless
// Framework industry standard. Without explicit throttling a single route can
// exhaust the entire account-level limit (10,000 rps shared across all APIs).
type HttpApiThrottling struct {
	// RateLimit is the maximum sustained requests per second.
	// Default: 1000.
	RateLimit int `pulumi:"rateLimit,optional"`

	// BurstLimit is the maximum concurrent requests.
	// Default: 500.
	BurstLimit int `pulumi:"burstLimit,optional"`
}

// ── CORS ───────────────────────────────────────────────────────────────────

// HttpApiCors configures Cross-Origin Resource Sharing at the API level.
// CORS is opt-in — omit to disable entirely.
//
// Security rules enforced by Anvil:
//   - allowOrigins "*" is blocked — forces explicit origin declaration
//   - allowCredentials: true requires explicit origins (never wildcard)
//   - allowMethods is inferred from routes automatically if omitted
//   - allowHeaders defaults to Content-Type, Authorization, X-Request-ID
type HttpApiCors struct {
	// AllowOrigins is the list of allowed origins.
	// Required — no default. Wildcard "*" is blocked as a security measure.
	// Example: ["https://app.mysite.com", "https://www.mysite.com"]
	AllowOrigins []string `pulumi:"allowOrigins"`

	// AllowHeaders is the list of allowed request headers.
	// Default: ["Content-Type", "Authorization", "X-Request-ID"]
	AllowHeaders []string `pulumi:"allowHeaders,optional"`

	// AllowMethods is the list of allowed HTTP methods.
	// Default: inferred from the routes declared on this API.
	AllowMethods []string `pulumi:"allowMethods,optional"`

	// AllowCredentials allows cookies and auth headers in cross-origin requests.
	// Default: false. When true, allowOrigins must not contain "*".
	AllowCredentials bool `pulumi:"allowCredentials,optional"`

	// MaxAge is the preflight cache duration in seconds.
	// Default: 86400 (24 hours) — reduces preflight requests significantly.
	MaxAge int `pulumi:"maxAge,optional"`
}

// ── Domain ─────────────────────────────────────────────────────────────────

// HttpApiDomain configures a custom domain for the HTTP API.
// When set, Anvil provisions an ACM certificate, API Gateway domain name,
// and Route 53 DNS record automatically.
// The raw execute-api endpoint is disabled when a domain is configured —
// all traffic must flow through the custom domain (and any WAF/proxy in front).
type HttpApiDomain struct {
	// Name is the fully qualified domain name, e.g. "api.mysite.com".
	Name string `pulumi:"name"`

	// BasePath is the optional API mapping base path, e.g. "v1".
	// When set the API is accessible at https://name/basePath.
	// Omit for root path mapping.
	BasePath string `pulumi:"basePath,optional"`

	// CertificateArn is a BYO ACM certificate ARN.
	// When omitted Anvil creates and validates the certificate automatically.
	CertificateArn string `pulumi:"certificateArn,optional"`

	// ZoneId is the Route 53 hosted zone ID.
	// When omitted Anvil discovers the zone by name automatically.
	ZoneId string `pulumi:"zoneId,optional"`

	// Dns controls whether Anvil creates Route 53 DNS and cert validation records.
	// Default: true. Set to false for Cloudflare or other non-Route 53 providers.
	// When false, Anvil outputs ApiGatewayDomainTarget and (if no certificateArn
	// is provided) CertValidationCname for manual configuration in your DNS provider.
	Dns *bool `pulumi:"dns,optional"`
}

// ── Args ───────────────────────────────────────────────────────────────────

// HttpApiArgs defines the inputs for an Anvil-managed HTTP API Gateway.
type HttpApiArgs struct {
	// Routes defines the API routes. Each route maps a method + path to a consumer.
	// At least one route is required.
	Routes []HttpApiRoute `pulumi:"routes"`

	// Domain configures a custom domain for the API.
	// When set, Anvil provisions the ACM cert, domain name, and DNS record.
	// The raw execute-api endpoint is disabled when a domain is set.
	// Omit to use the default API Gateway URL (execute-api endpoint stays enabled).
	Domain *HttpApiDomain `pulumi:"domain,optional"`

	// Cors configures Cross-Origin Resource Sharing.
	// Opt-in — omit to disable CORS entirely.
	// When enabled, allowOrigins is required and "*" is blocked.
	Cors *HttpApiCors `pulumi:"cors,optional"`

	// Throttling configures rate and burst limits for all routes.
	// Optional — defaults to rateLimit: 1000, burstLimit: 500 when omitted.
	// Set to override defaults. Applied to all routes unless overridden per-route.
	Throttling *HttpApiThrottling `pulumi:"throttling,optional"`

	// LogRetention sets the CloudWatch access log retention period.
	// Presets: "7d" | "30d" | "90d" | "1y" | "3y" | "6y" | "7y". Default: "1y".
	LogRetention string `pulumi:"logRetention,optional"`

	// DefaultAuthorizerId is the JWT authorizer to apply to all routes; HttpApi creates it on this API.
	// Pass auth.authorizerId from an OAuthAuthorizer or CognitoAuth component.
	// All routes inherit this authorizer unless skipAuth: true is set on the route.
	// Omit to leave all routes public (no auth).
	DefaultAuthorizerId pulumi.StringInput `pulumi:"defaultAuthorizerId,optional"`
}

// ── Component ──────────────────────────────────────────────────────────────

// HttpApi is the Anvil-managed HTTP API Gateway component resource.
type HttpApi struct {
	pulumi.ResourceState

	// ApiId is the API Gateway HTTP API ID.
	ApiId pulumi.StringOutput `pulumi:"apiId"`

	// ApiEndpoint is the default execute-api endpoint URL.
	// Empty string when a custom domain is set and the endpoint is disabled.
	ApiEndpoint pulumi.StringOutput `pulumi:"apiEndpoint"`

	// Url is the primary URL for the API.
	// When a custom domain is configured this is the custom domain URL.
	// Otherwise it is the execute-api endpoint URL.
	Url pulumi.StringOutput `pulumi:"url"`

	// CertValidationCname is the ACM cert validation CNAME record.
	// Only populated when dns: false and certificateArn is omitted.
	// Add this record in Cloudflare then re-run deploy.
	CertValidationCname pulumi.AnyOutput `pulumi:"certValidationCname"`

	name string
}

func (h *HttpApi) Annotate(a infer.Annotator) {
	a.SetToken("aws", "HttpApi")
	a.Describe(&h, "An Anvil-managed AWS HTTP API Gateway (API Gateway v2). Route-level consumers support Lambda, SQS, EventBridge, Step Functions, and HTTP proxy integrations. Secure by default: TLS 1.2 minimum enforced, execute-api endpoint disabled when a custom domain is set, conservative throttling defaults, CORS opt-in with wildcard origin blocked.")
}

// NewHttpApi creates a new Anvil-managed HTTP API Gateway component.
//
// Security model:
//   - TLS 1.2 minimum enforced on custom domains (no escape hatch)
//   - Execute-api endpoint disabled when custom domain is set
//   - Throttling defaults: 1000 rps rate, 500 burst
//   - CORS opt-in, wildcard origin blocked, credentials default false
//   - Per-consumer IAM roles with least-privilege permissions
//   - Access logs on by default, 1y retention
func NewHttpApi(ctx *pulumi.Context, name string, args HttpApiArgs, opts ...pulumi.ResourceOption) (*HttpApi, error) {
	h := &HttpApi{name: name}

	provider.NewContext(ctx)

	cfg := c.New(ctx, "anvil")
	stage := cfg.Require("stage")
	stageId := cfg.Require("stageId")

	opts = provider.WithDefault(opts, true)

	if err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, h, opts...); err != nil {
		return nil, err
	}

	// ── Validation ─────────────────────────────────────────────────────────

	if len(args.Routes) == 0 {
		return nil, fmt.Errorf("HttpApi %q: at least one route is required", name)
	}

	// Validate all routes and build method list for CORS inference.
	routeMethods := make([]string, 0, len(args.Routes))
	for i, route := range args.Routes {
		if err := apigateway.ValidateMethod(route.Method); err != nil {
			return nil, fmt.Errorf("HttpApi %q route[%d]: %w", name, i, err)
		}
		consumerType, err := route.Consumer.consumerType()
		if err != nil {
			return nil, fmt.Errorf("HttpApi %q route[%d] (%s %s): %w", name, i, route.Method, route.Path, err)
		}
		if err := apigateway.ValidateConsumerMethod(route.Method, consumerType); err != nil {
			return nil, fmt.Errorf("HttpApi %q route[%d]: %w", name, i, err)
		}
		if route.Path == "" {
			return nil, fmt.Errorf("HttpApi %q route[%d]: path is required", name, i)
		}
		routeMethods = append(routeMethods, route.Method)
	}

	// Validate CORS configuration.
	if args.Cors != nil {
		if len(args.Cors.AllowOrigins) == 0 {
			return nil, fmt.Errorf("HttpApi %q: cors.allowOrigins is required when CORS is enabled", name)
		}
		for _, origin := range args.Cors.AllowOrigins {
			if origin == "*" {
				return nil, fmt.Errorf(
					"HttpApi %q: cors.allowOrigins \"*\" is not allowed — specify explicit origins. "+
						"Wildcard origins disable same-origin protection and fail SOC 2 / PCI DSS audits",
					name,
				)
			}
		}
		if args.Cors.AllowCredentials {
			for _, origin := range args.Cors.AllowOrigins {
				if origin == "*" {
					return nil, fmt.Errorf(
						"HttpApi %q: cors.allowCredentials cannot be true when allowOrigins contains \"*\" — "+
							"browsers reject this combination per the CORS specification",
						name,
					)
				}
			}
		}
	}

	// Validate domain DNS configuration.
	if args.Domain != nil {
		skipDns := args.Domain.Dns != nil && !*args.Domain.Dns
		if skipDns && args.Domain.ZoneId != "" {
			return nil, fmt.Errorf(
				"HttpApi %q: domain.zoneId has no effect when domain.dns is false — "+
					"remove zoneId or set dns: true",
				name,
			)
		}
	}

	// ── Throttling defaults ────────────────────────────────────────────────

	throttleRate := 1000
	throttleBurst := 500
	if args.Throttling != nil {
		if args.Throttling.RateLimit > 0 {
			throttleRate = args.Throttling.RateLimit
		}
		if args.Throttling.BurstLimit > 0 {
			throttleBurst = args.Throttling.BurstLimit
		}
	}

	// ── Shared base (logs + domain) ────────────────────────────────────────

	var domainCfg *apigateway.DomainConfig
	if args.Domain != nil {
		domainCfg = &apigateway.DomainConfig{
			Name:           args.Domain.Name,
			BasePath:       args.Domain.BasePath,
			CertificateArn: args.Domain.CertificateArn,
			ZoneId:         args.Domain.ZoneId,
			Dns:            args.Domain.Dns,
		}
	}

	logRetentionDays := apigateway.ResolveDays(args.LogRetention)

	base, err := apigateway.SetupBase(ctx, name, stage, stageId, domainCfg, logRetentionDays, h)
	if err != nil {
		return nil, err
	}

	// ── CORS configuration ─────────────────────────────────────────────────

	var corsConfig *apigatewayv2.ApiCorsConfigurationArgs
	if args.Cors != nil {
		allowHeaders := args.Cors.AllowHeaders
		if len(allowHeaders) == 0 {
			allowHeaders = []string{"Content-Type", "Authorization", "X-Request-ID"}
		}

		allowMethods := args.Cors.AllowMethods
		if len(allowMethods) == 0 {
			allowMethods = apigateway.InferAllowMethods(routeMethods)
		}

		maxAge := args.Cors.MaxAge
		if maxAge == 0 {
			maxAge = 86400
		}

		allowOriginsInput := make(pulumi.StringArray, len(args.Cors.AllowOrigins))
		for i, o := range args.Cors.AllowOrigins {
			allowOriginsInput[i] = pulumi.String(o)
		}
		allowHeadersInput := make(pulumi.StringArray, len(allowHeaders))
		for i, h := range allowHeaders {
			allowHeadersInput[i] = pulumi.String(h)
		}
		allowMethodsInput := make(pulumi.StringArray, len(allowMethods))
		for i, m := range allowMethods {
			allowMethodsInput[i] = pulumi.String(m)
		}

		corsConfig = &apigatewayv2.ApiCorsConfigurationArgs{
			AllowOrigins:     allowOriginsInput,
			AllowHeaders:     allowHeadersInput,
			AllowMethods:     allowMethodsInput,
			AllowCredentials: pulumi.Bool(args.Cors.AllowCredentials),
			MaxAge:           pulumi.Int(maxAge),
		}
	}

	// ── HTTP API resource ──────────────────────────────────────────────────

	apiName := provider.PhysicalName(stage, name, "httpapi", stageId)

	apiArgs := &apigatewayv2.ApiArgs{
		Name:         pulumi.String(apiName),
		ProtocolType: pulumi.String("HTTP"),
		Tags: pulumi.StringMap{
			"ManagedBy": pulumi.String("anvil"),
		},
	}

	// Disable the raw execute-api endpoint when a custom domain is set.
	// This forces all traffic through the custom domain (and any Cloudflare
	// WAF / proxy in front), closing the origin bypass vector.
	if args.Domain != nil {
		apiArgs.DisableExecuteApiEndpoint = pulumi.Bool(true)
	}

	if corsConfig != nil {
		apiArgs.CorsConfiguration = corsConfig
	}

	api, err := apigatewayv2.NewApi(ctx, name+"-api", apiArgs, pulumi.Parent(h))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP API: %w", err)
	}

	// ── $default stage with auto-deploy ───────────────────────────────────
	//
	// Always use $default stage — users never need to think about stages.
	// Anvil handles environment separation via separate Pulumi stacks, not stages.
	// Auto-deploy ensures changes take effect immediately without manual deploys.
	//
	// Throttling is applied at the stage level and inherited by all routes
	// unless overridden per-route.

	stageArgs := &apigatewayv2.StageArgs{
		ApiId:      api.ID(),
		Name:       pulumi.String("$default"),
		AutoDeploy: pulumi.Bool(true),
		AccessLogSettings: &apigatewayv2.StageAccessLogSettingsArgs{
			DestinationArn: base.LogGroupArn,
			// Structured JSON access log format capturing all fields required
			// by SOC 2, ISO 27001, and PCI DSS auditors. Anvil owns this format —
			// users configure retention only.
			Format: pulumi.String(apigateway.DefaultLogFormat),
		},
		DefaultRouteSettings: &apigatewayv2.StageDefaultRouteSettingsArgs{
			ThrottlingRateLimit:  pulumi.Float64(float64(throttleRate)),
			ThrottlingBurstLimit: pulumi.Int(throttleBurst),
			// Detailed metrics enabled — surfaces per-route latency and error
			// rates in CloudWatch without additional configuration.
			DetailedMetricsEnabled: pulumi.Bool(true),
		},
		Tags: pulumi.StringMap{
			"ManagedBy": pulumi.String("anvil"),
		},
	}

	apiStage, err := apigatewayv2.NewStage(ctx, name+"-stage", stageArgs, pulumi.Parent(h))
	if err != nil {
		return nil, fmt.Errorf("failed to create API stage: %w", err)
	}

	// ── JWT authorizer ─────────────────────────────────────────────────────
	// Authorizers belong to a single API, so the authorizer described by an
	// OAuthAuthorizer/CognitoAuth reference is created here, on this API.

	var defaultAuthorizerId pulumi.StringInput
	if args.DefaultAuthorizerId != nil {
		authorizer, err := apigateway.NewJwtAuthorizer(ctx, name+"-authorizer", api.ID(), args.DefaultAuthorizerId, h)
		if err != nil {
			return nil, fmt.Errorf("failed to create JWT authorizer: %w", err)
		}
		defaultAuthorizerId = authorizer.ID().ToStringOutput()
	}

	// ── Routes and integrations ────────────────────────────────────────────

	for i, route := range args.Routes {
		routeSuffix := fmt.Sprintf("%s-%s-%s", apigateway.SanitizeRouteName(route.Method), apigateway.SanitizeRouteName(route.Path), fmt.Sprintf("%d", i))

		consumerType, _ := route.Consumer.consumerType() // already validated above

		switch consumerType {
		case "lambda":
			if err := wireLambdaIntegration(ctx, name, routeSuffix, api, apiStage, route, defaultAuthorizerId, h); err != nil {
				return nil, err
			}
		case "sqs":
			if err := wireSqsIntegration(ctx, name, routeSuffix, api, apiStage, route, stage, stageId, defaultAuthorizerId, h); err != nil {
				return nil, err
			}
		case "eventBridge":
			if err := wireEventBridgeIntegration(ctx, name, routeSuffix, api, apiStage, route, stage, stageId, defaultAuthorizerId, h); err != nil {
				return nil, err
			}
		case "stepFunctions":
			if err := wireStepFunctionsIntegration(ctx, name, routeSuffix, api, apiStage, route, stage, stageId, defaultAuthorizerId, h); err != nil {
				return nil, err
			}
		case "http":
			if err := wireHttpIntegration(ctx, name, routeSuffix, api, apiStage, route, defaultAuthorizerId, h); err != nil {
				return nil, err
			}
		}
	}

	// ── API mapping (custom domain) ────────────────────────────────────────

	if args.Domain != nil {
		basePath := ""
		if args.Domain.BasePath != "" {
			basePath = args.Domain.BasePath
		}

		_, err := apigatewayv2.NewApiMapping(ctx, name+"-mapping", &apigatewayv2.ApiMappingArgs{
			ApiId:         api.ID(),
			DomainName:    base.DomainNameId,
			Stage:         apiStage.ID(),
			ApiMappingKey: pulumi.String(basePath),
		}, pulumi.Parent(h))
		if err != nil {
			return nil, fmt.Errorf("failed to create API mapping: %w", err)
		}
	}

	// ── Outputs ────────────────────────────────────────────────────────────

	h.ApiId = api.ID().ToStringOutput()
	h.ApiEndpoint = api.ApiEndpoint

	if args.Domain != nil {
		prefix := "https://"
		suffix := ""
		if args.Domain.BasePath != "" {
			suffix = "/" + args.Domain.BasePath
		}
		h.Url = pulumi.Sprintf("%s%s%s", prefix, args.Domain.Name, suffix)
	} else {
		h.Url = api.ApiEndpoint
	}

	h.CertValidationCname = base.CertValidationCname

	ctx.RegisterResourceOutputs(h, pulumi.Map{
		"apiId":               api.ID(),
		"apiEndpoint":         api.ApiEndpoint,
		"url":                 h.Url,
		"certValidationCname": base.CertValidationCname,
	})

	return h, nil
}

// ── Integration wiring ─────────────────────────────────────────────────────

// wireLambdaIntegration creates an AWS_PROXY Lambda integration and route.
// API Gateway invokes the Lambda synchronously and returns its response directly.
func wireLambdaIntegration(
	ctx *pulumi.Context,
	apiName string,
	routeSuffix string,
	api *apigatewayv2.Api,
	stage *apigatewayv2.Stage,
	route HttpApiRoute,
	defaultAuthorizerId pulumi.StringInput,
	parent pulumi.Resource,
) error {
	lambdaArn := route.Consumer.Lambda.Arn.ToStringOutput()

	// Integration URI for Lambda proxy uses the invocation ARN format.
	integrationUri := lambdaArn.ApplyT(func(arn string) string {
		return arn
	}).(pulumi.StringOutput)

	integration, err := apigatewayv2.NewIntegration(ctx, apiName+"-int-"+routeSuffix, &apigatewayv2.IntegrationArgs{
		ApiId:                api.ID(),
		IntegrationType:      pulumi.String("AWS_PROXY"),
		IntegrationUri:       integrationUri,
		IntegrationMethod:    pulumi.String("POST"),
		PayloadFormatVersion: pulumi.String("2.0"),
		TimeoutMilliseconds:  pulumi.Int(29000),
	}, pulumi.Parent(parent))
	if err != nil {
		return fmt.Errorf("failed to create Lambda integration for route %s %s: %w", route.Method, route.Path, err)
	}

	// Lambda resource-based permission allowing API Gateway to invoke the function.
	// Uses the typed awslambda.Permission resource — RegisterResource with a raw map
	// silently fails to create the policy in some provider versions.
	_, err = awslambda.NewPermission(ctx, apiName+"-perm-"+routeSuffix, &awslambda.PermissionArgs{
		Action:    pulumi.String("lambda:InvokeFunction"),
		Function:  lambdaArn,
		Principal: pulumi.String("apigateway.amazonaws.com"),
		SourceArn: pulumi.Sprintf("%s/*/*", api.ExecutionArn),
	}, pulumi.Parent(parent))
	if err != nil {
		return fmt.Errorf("failed to create Lambda invoke permission for route %s %s: %w", route.Method, route.Path, err)
	}

	return createRoute(ctx, apiName, routeSuffix, api, stage, route, integration.ID().ToStringOutput(), defaultAuthorizerId, parent)
}

// wireSqsIntegration creates a direct SQS integration.
// API Gateway sends the request body as an SQS message — no Lambda required.
// Returns 202 Accepted immediately (async pattern).
// IAM role grants sqs:SendMessage on the specific queue ARN only.
func wireSqsIntegration(
	ctx *pulumi.Context,
	apiName string,
	routeSuffix string,
	api *apigatewayv2.Api,
	stage *apigatewayv2.Stage,
	route HttpApiRoute,
	appStage string,
	stageId string,
	defaultAuthorizerId pulumi.StringInput,
	parent pulumi.Resource,
) error {
	queueArn := route.Consumer.Sqs.Arn.ToStringOutput()

	// Least-privilege IAM role — sqs:SendMessage on this queue only.
	role, err := createIntegrationRole(ctx, apiName+"-sqs-role-"+routeSuffix, pulumi.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": "sqs:SendMessage",
			"Resource": %q
		}]
	}`, queueArn), appStage, stageId, parent)
	if err != nil {
		return fmt.Errorf("failed to create SQS integration role: %w", err)
	}

	queueUrl := route.Consumer.Sqs.Url.ToStringOutput()

	integration, err := apigatewayv2.NewIntegration(ctx, apiName+"-int-"+routeSuffix, &apigatewayv2.IntegrationArgs{
		ApiId:              api.ID(),
		IntegrationType:    pulumi.String("AWS_PROXY"),
		IntegrationSubtype: pulumi.String("SQS-SendMessage"),
		CredentialsArn:     role.Arn,
		RequestParameters: pulumi.StringMap{
			"QueueUrl":    queueUrl,
			"MessageBody": pulumi.String("$request.body"),
		},
		PayloadFormatVersion: pulumi.String("1.0"),
		TimeoutMilliseconds:  pulumi.Int(29000),
	}, pulumi.Parent(parent))
	if err != nil {
		return fmt.Errorf("failed to create SQS integration for route %s %s: %w", route.Method, route.Path, err)
	}

	return createRoute(ctx, apiName, routeSuffix, api, stage, route, integration.ID().ToStringOutput(), defaultAuthorizerId, parent)
}

// wireEventBridgeIntegration creates a direct EventBridge integration.
// API Gateway puts the request body as an event — no Lambda required.
// IAM role grants events:PutEvents on the specific bus only.
func wireEventBridgeIntegration(
	ctx *pulumi.Context,
	apiName string,
	routeSuffix string,
	api *apigatewayv2.Api,
	stage *apigatewayv2.Stage,
	route HttpApiRoute,
	appStage string,
	stageId string,
	defaultAuthorizerId pulumi.StringInput,
	parent pulumi.Resource,
) error {
	busName := route.Consumer.EventBridge.Name.ToStringOutput()

	role, err := createIntegrationRole(ctx, apiName+"-eb-role-"+routeSuffix, pulumi.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": "events:PutEvents",
			"Resource": "arn:aws:events:*:*:event-bus/%s"
		}]
	}`, busName), appStage, stageId, parent)
	if err != nil {
		return fmt.Errorf("failed to create EventBridge integration role: %w", err)
	}

	integration, err := apigatewayv2.NewIntegration(ctx, apiName+"-int-"+routeSuffix, &apigatewayv2.IntegrationArgs{
		ApiId:              api.ID(),
		IntegrationType:    pulumi.String("AWS_PROXY"),
		IntegrationSubtype: pulumi.String("EventBridge-PutEvents"),
		CredentialsArn:     role.Arn,
		RequestParameters: pulumi.StringMap{
			"Detail":       pulumi.String("$request.body"),
			"DetailType":   pulumi.String("ApiGatewayEvent"),
			"EventBusName": busName,
			"Source":       pulumi.String("anvil.api"),
		},
		PayloadFormatVersion: pulumi.String("1.0"),
		TimeoutMilliseconds:  pulumi.Int(29000),
	}, pulumi.Parent(parent))
	if err != nil {
		return fmt.Errorf("failed to create EventBridge integration for route %s %s: %w", route.Method, route.Path, err)
	}

	return createRoute(ctx, apiName, routeSuffix, api, stage, route, integration.ID().ToStringOutput(), defaultAuthorizerId, parent)
}

// wireStepFunctionsIntegration creates a direct Step Functions integration.
// Async (StartExecution) by default. Set consumer.Sync: true for sync execution.
// IAM role grants states:StartExecution or states:StartSyncExecution on the specific ARN.
func wireStepFunctionsIntegration(
	ctx *pulumi.Context,
	apiName string,
	routeSuffix string,
	api *apigatewayv2.Api,
	stage *apigatewayv2.Stage,
	route HttpApiRoute,
	appStage string,
	stageId string,
	defaultAuthorizerId pulumi.StringInput,
	parent pulumi.Resource,
) error {
	smArn := route.Consumer.StepFunctions.Arn.ToStringOutput()
	isSync := route.Consumer.StepFunctions.Sync

	action := "states:StartExecution"
	integrationSubtype := "StepFunctions-StartExecution"
	if isSync {
		action = "states:StartSyncExecution"
		integrationSubtype = "StepFunctions-StartSyncExecution"
	}

	role, err := createIntegrationRole(ctx, apiName+"-sf-role-"+routeSuffix, pulumi.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": "%s",
			"Resource": "%s"
		}]
	}`, action, smArn), appStage, stageId, parent)
	if err != nil {
		return fmt.Errorf("failed to create Step Functions integration role: %w", err)
	}

	integration, err := apigatewayv2.NewIntegration(ctx, apiName+"-int-"+routeSuffix, &apigatewayv2.IntegrationArgs{
		ApiId:              api.ID(),
		IntegrationType:    pulumi.String("AWS_PROXY"),
		IntegrationSubtype: pulumi.String(integrationSubtype),
		CredentialsArn:     role.Arn,
		RequestParameters: pulumi.StringMap{
			"Input":           pulumi.String("$request.body"),
			"StateMachineArn": smArn,
		},
		PayloadFormatVersion: pulumi.String("1.0"),
		TimeoutMilliseconds:  pulumi.Int(29000),
	}, pulumi.Parent(parent))
	if err != nil {
		return fmt.Errorf("failed to create Step Functions integration for route %s %s: %w", route.Method, route.Path, err)
	}

	return createRoute(ctx, apiName, routeSuffix, api, stage, route, integration.ID().ToStringOutput(), defaultAuthorizerId, parent)
}

// wireHttpIntegration creates an HTTP_PROXY integration to an external URL.
// Requests are forwarded as-is to the target URL. No IAM needed.
func wireHttpIntegration(
	ctx *pulumi.Context,
	apiName string,
	routeSuffix string,
	api *apigatewayv2.Api,
	stage *apigatewayv2.Stage,
	route HttpApiRoute,
	defaultAuthorizerId pulumi.StringInput,
	parent pulumi.Resource,
) error {
	integration, err := apigatewayv2.NewIntegration(ctx, apiName+"-int-"+routeSuffix, &apigatewayv2.IntegrationArgs{
		ApiId:                api.ID(),
		IntegrationType:      pulumi.String("HTTP_PROXY"),
		IntegrationUri:       pulumi.String(route.Consumer.Http.Url),
		IntegrationMethod:    pulumi.String(route.Method),
		PayloadFormatVersion: pulumi.String("1.0"),
		TimeoutMilliseconds:  pulumi.Int(29000),
	}, pulumi.Parent(parent))
	if err != nil {
		return fmt.Errorf("failed to create HTTP proxy integration for route %s %s: %w", route.Method, route.Path, err)
	}

	return createRoute(ctx, apiName, routeSuffix, api, stage, route, integration.ID().ToStringOutput(), defaultAuthorizerId, parent)
}

// createRoute creates the API Gateway v2 route resource linking a method+path to an integration.
// When defaultAuthorizerId is set, all routes inherit JWT auth unless skipAuth is true.
// Per-route scopes are applied when configured.
func createRoute(
	ctx *pulumi.Context,
	apiName string,
	routeSuffix string,
	api *apigatewayv2.Api,
	stage *apigatewayv2.Stage,
	route HttpApiRoute,
	integrationId pulumi.StringOutput,
	defaultAuthorizerId pulumi.StringInput,
	parent pulumi.Resource,
) error {
	routeKey := fmt.Sprintf("%s %s", route.Method, route.Path)
	if route.Method == "ANY" {
		routeKey = "$default"
	}

	routeArgs := &apigatewayv2.RouteArgs{
		ApiId:    api.ID(),
		RouteKey: pulumi.String(routeKey),
		Target: integrationId.ApplyT(func(id string) string {
			return "integrations/" + id
		}).(pulumi.StringOutput),
	}

	if defaultAuthorizerId != nil {
		if route.SkipAuth {
			// Explicit opt-out — route is public even though a default authorizer is set.
			routeArgs.AuthorizationType = pulumi.StringPtr("NONE")
		} else {
			// Inherit the default authorizer.
			routeArgs.AuthorizationType = pulumi.StringPtr("JWT")
			routeArgs.AuthorizerId = defaultAuthorizerId.ToStringOutput()

			if len(route.Scopes) > 0 {
				scopes := make(pulumi.StringArray, len(route.Scopes))
				for i, s := range route.Scopes {
					scopes[i] = pulumi.String(s)
				}
				routeArgs.AuthorizationScopes = scopes
			}
		}
	}

	_, err := apigatewayv2.NewRoute(ctx, apiName+"-route-"+routeSuffix, routeArgs, pulumi.Parent(parent))
	if err != nil {
		return fmt.Errorf("failed to create route %s %s: %w", route.Method, route.Path, err)
	}

	return nil
}

// createIntegrationRole creates a least-privilege IAM role for an API Gateway
// service integration. API Gateway assumes this role to call SQS, EventBridge,
// or Step Functions on behalf of the request.
func createIntegrationRole(
	ctx *pulumi.Context,
	roleName string,
	policyJSON pulumi.StringInput,
	stage string,
	stageId string,
	parent pulumi.Resource,
) (*iam.Role, error) {
	assumeRolePolicy := `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Principal": { "Service": "apigateway.amazonaws.com" },
			"Action": "sts:AssumeRole"
		}]
	}`

	role, err := iam.NewRole(ctx, roleName, &iam.RoleArgs{
		AssumeRolePolicy: pulumi.String(assumeRolePolicy),
		Tags: pulumi.StringMap{
			"ManagedBy": pulumi.String("anvil"),
		},
	}, pulumi.Parent(parent))
	if err != nil {
		return nil, fmt.Errorf("failed to create integration IAM role: %w", err)
	}

	_, err = iam.NewRolePolicy(ctx, roleName+"-policy", &iam.RolePolicyArgs{
		Role:   role.Name,
		Policy: policyJSON,
	}, pulumi.Parent(parent))
	if err != nil {
		return nil, fmt.Errorf("failed to attach integration IAM policy: %w", err)
	}

	return role, nil
}
