package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"buf.build/go/protovalidate"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	grpclogging "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/recovery"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/validator"
	grpcprometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/multierr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	grpchealth "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/config"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/controller/api"
	extproc "github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/controller/extproc"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/guardrails/demask"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/health"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/logging"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/models"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/repository"
	storefactory "github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/repository/factory"
	storeredis "github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/repository/redis"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/repository/statecodec"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/service/audit"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/service/rulesreload"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/service/settings"
	maskuc "github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/usecases/guardrails/mask"
	scanuc "github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/usecases/guardrails/scan"
	rulesuc "github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/usecases/rules"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/usecases/rules/builtins"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/version"
	gregistry "github.com/cloud-ru-tech/guardrails-llm-filter-extproc/pkg/guardrails/regex/registry"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/pkg/guardrails/regex/rule"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/pkg/guardrails/regex/scanners/placeholder"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/pkg/guardrails/regex/scanners/sensitive"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/pkg/tlsutils"

	servicev1 "github.com/cloud-ru-tech/guardrails-llm-filter-extproc/pkg/api/proto/cloudru/guardrails/v1/service"
)

// Extproc represents the main application structure
type Extproc struct {
	cfg *config.Config

	grpcServer           *grpc.Server
	managementGrpcServer *grpc.Server
	metricsServer        *http.Server
	healthServer         *grpc.Server
	apiServer            *http.Server
	extproc              *extproc.Controller
	grpcController       *api.Controller
	stop                 func() error

	maskUC *maskuc.UseCase

	demaskerProvider *demask.Provider

	store              repository.Store
	codec              statecodec.Codec
	settingsService    *settings.Service
	rulesUC            *rulesuc.UseCase
	builtinsIndex      *builtins.Index
	rulesReloader      *rulesreload.Reloader
	auditRecorder      *audit.Recorder
	dataTypes          []rule.DataType
	fileRules          []rule.Rule
	guardrailsRegistry *gregistry.Reloadable
	sensitiveScanner   *sensitive.Scanner
	placeholderScanner *placeholder.Scanner
}

// New creates a new Extproc instance.
func New(cfg *config.Config) *Extproc {
	return &Extproc{cfg: cfg}
}

// Store returns the configured persistence backend (masking state, custom
// rules, global settings). Panics on misconfiguration or unreachable
// external backend: this is a boot-time error.
func (e *Extproc) Store() repository.Store {
	if e.store != nil {
		return e.store
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := storefactory.New(ctx, storefactory.Config{
		Backend:         storefactory.Backend(e.cfg.Store.Backend),
		MaskingTTL:      e.cfg.Store.MaskingTTL,
		AuditTTL:        e.cfg.Audit.Retention,
		AuditMaxEntries: e.cfg.Audit.MaxEntries,
		Redis: storeredis.Config{
			Addr:     e.cfg.Store.Redis.Addr,
			Password: e.cfg.Store.Redis.Password,
			DB:       e.cfg.Store.Redis.DB,
		},
		PostgresDSN:       e.cfg.Store.PostgresDSN,
		EncryptionEnabled: e.cfg.Store.EncryptionEnabled,
		EncryptionKey:     e.cfg.Store.EncryptionKey,
	})
	if err != nil {
		panic(fmt.Errorf("create store (backend %q): %w", e.cfg.Store.Backend, err))
	}
	e.store = st
	return e.store
}

// storeCodec returns the statecodec used for at-rest encryption, matching the
// store's own configuration. It is shared with the audit recorder (to seal
// originals) and the API controller (to open them). Same key as the store
// factory builds internally, so envelopes are mutually decryptable. Panics on
// a misconfigured key: a boot-time error (config.Load already rejects
// encrypted originals without a key, so this is defense in depth).
func (e *Extproc) storeCodec() statecodec.Codec {
	if e.codec != nil {
		return e.codec
	}
	if e.cfg.Store.EncryptionEnabled {
		c, err := statecodec.NewAESGCMFromBase64(e.cfg.Store.EncryptionKey)
		if err != nil {
			panic(fmt.Errorf("build store codec: %w", err))
		}
		e.codec = c
	} else {
		e.codec = statecodec.Plain()
	}
	return e.codec
}

// SettingsService returns the global guardrails settings service.
func (e *Extproc) SettingsService() *settings.Service {
	if e.settingsService != nil {
		return e.settingsService
	}

	// An empty GUARDRAILS_DATA_TYPES seeds an empty enabled-types set (masking
	// effectively off until configured via the API) rather than crashing the
	// process. ParseDataTypes errors on empty input, so guard it here.
	var dataTypes []models.DataType
	if strings.TrimSpace(e.cfg.Guardrails.DataTypes) != "" {
		parsed, err := settings.ParseDataTypes(e.cfg.Guardrails.DataTypes)
		if err != nil {
			panic(fmt.Errorf("parse GUARDRAILS_DATA_TYPES %q: %w", e.cfg.Guardrails.DataTypes, err))
		}
		dataTypes = parsed
	}
	mode, err := models.ParseGuardrailsMode(e.cfg.Guardrails.Mode)
	if err != nil {
		panic(fmt.Errorf("parse GUARDRAILS_MODE: %w", err))
	}

	e.settingsService = settings.New(e.Store(), models.GuardrailsSettings{
		Enabled:   e.cfg.Guardrails.Enabled,
		DataTypes: dataTypes,
		Mode:      mode,
	})
	return e.settingsService
}

// RulesUseCase returns the rule-management use-case coordinator (custom-rule
// CRUD, enable/disable, read/list) backing the configuration API.
func (e *Extproc) RulesUseCase() *rulesuc.UseCase {
	if e.rulesUC != nil {
		return e.rulesUC
	}
	e.rulesUC = rulesuc.NewUseCase(rulesuc.Deps{
		Store:          e.Store(),
		Builtins:       e.BuiltinsIndex(),
		Reloader:       e.RulesReloader(),
		MaxCustomRules: e.cfg.GuardrailsRules.MaxCustom,
		MaxPatternLen:  e.cfg.GuardrailsRules.MaxPatternLen,
	})
	return e.rulesUC
}

// BuiltinsIndex returns the immutable index over the built-in rules loaded
// from the YAML files, shared by the rule use cases.
func (e *Extproc) BuiltinsIndex() *builtins.Index {
	if e.builtinsIndex != nil {
		return e.builtinsIndex
	}
	e.builtinsIndex = builtins.New(e.loadFileRules())
	return e.builtinsIndex
}

// RulesReloader returns the registry merge-and-swap service shared by the
// boot-time load, the refresh ticker and the rule-mutation use cases.
func (e *Extproc) RulesReloader() *rulesreload.Reloader {
	if e.rulesReloader != nil {
		return e.rulesReloader
	}
	e.rulesReloader = rulesreload.New(e.loadFileRules(), e.Store(), e.GuardrailsRegistry())
	return e.rulesReloader
}

// DataTypes returns the data-type groups declared in the rule files.
func (e *Extproc) DataTypes() []rule.DataType {
	e.loadFileRules()
	return e.dataTypes
}

func (e *Extproc) loadFileRules() []rule.Rule {
	if e.fileRules != nil {
		return e.fileRules
	}
	dataTypes, rules, err := rule.LoadAllFromFiles(
		e.cfg.GuardrailsRules.RegexRulesFile,
		e.cfg.GuardrailsRules.GitleaksRegexRulesFile,
	)
	if err != nil {
		panic(fmt.Errorf(
			"load guardrails rules from %s and %s: %w",
			e.cfg.GuardrailsRules.RegexRulesFile,
			e.cfg.GuardrailsRules.GitleaksRegexRulesFile,
			err,
		))
	}
	e.dataTypes = dataTypes
	e.fileRules = rules
	return e.fileRules
}

// GuardrailsRegistry returns the reloadable compiled-rule registry seeded
// with the file rules. Custom rules from the store are merged in by
// RulesReloader().Reload during Start.
func (e *Extproc) GuardrailsRegistry() *gregistry.Reloadable {
	if e.guardrailsRegistry != nil {
		return e.guardrailsRegistry
	}
	reg := gregistry.NewRegistry()
	reg.Register(e.loadFileRules()...)
	e.guardrailsRegistry = gregistry.NewReloadable(reg)
	return e.guardrailsRegistry
}

func (e *Extproc) SensitiveScanner() *sensitive.Scanner {
	if e.sensitiveScanner != nil {
		return e.sensitiveScanner
	}
	if e.cfg.Guardrails.KeywordPrefilterEnabled {
		// Surface which keyword-bearing rules are NOT pre-filtered (their regex
		// does not guarantee a keyword in every match) so operators can see what
		// stays fully scanned. Only rule IDs are logged — not sensitive.
		if ineligible := e.GuardrailsRegistry().PrefilterIneligibleRuleIDs(); len(ineligible) > 0 {
			logging.Info(context.Background(),
				"keyword pre-filter enabled; these rules declare keywords but are always scanned (regex does not guarantee a keyword in every match)",
				"count", len(ineligible),
				"rule_ids", ineligible,
			)
		}
	}
	e.sensitiveScanner = sensitive.New(
		e.GuardrailsRegistry(),
		sensitive.WithKeywordPrefilter(e.cfg.Guardrails.KeywordPrefilterEnabled),
	)
	return e.sensitiveScanner
}

func (e *Extproc) PlaceholderScanner() *placeholder.Scanner {
	if e.placeholderScanner != nil {
		return e.placeholderScanner
	}
	e.placeholderScanner = placeholder.New(e.GuardrailsRegistry())
	return e.placeholderScanner
}

func (e *Extproc) DemaskerProvider() *demask.Provider {
	if e.demaskerProvider != nil {
		return e.demaskerProvider
	}
	e.demaskerProvider = demask.NewProvider(
		e.GuardrailsRegistry(),
		e.PlaceholderScanner(),
	)
	return e.demaskerProvider
}

// MaskUseCase returns the mask use case.
func (e *Extproc) MaskUseCase() *maskuc.UseCase {
	if e.maskUC != nil {
		return e.maskUC
	}
	e.maskUC = maskuc.New(maskuc.Deps{
		Registry: e.GuardrailsRegistry(),
		Scanner:  e.SensitiveScanner(),
	}, maskuc.WithParallelMinBytes(e.cfg.Guardrails.MaskParallelMinBytes))
	return e.maskUC
}

// AuditRecorder returns the masking audit recorder, or nil when the audit
// trail is disabled.
func (e *Extproc) AuditRecorder() *audit.Recorder {
	if e.auditRecorder != nil {
		return e.auditRecorder
	}
	if !e.cfg.Audit.Enabled {
		return nil
	}
	e.auditRecorder = audit.New(e.Store(), e.GuardrailsRegistry(),
		e.cfg.Audit.StoreMaskedTexts, e.cfg.Audit.StoreMaskedResponseTexts,
		e.cfg.Audit.StoreOriginalTexts, e.storeCodec())
	return e.auditRecorder
}

// ExtprocController returns the extproc controller.
func (e *Extproc) ExtprocController() *extproc.Controller {
	if e.extproc != nil {
		return e.extproc
	}
	if e.cfg.Guardrails.StateKeySalt == "" {
		logging.Warn(context.Background(), "GUARDRAILS_STATE_KEY_SALT is empty; masking-state store keys use an unkeyed SHA-256 of x-request-id — set a shared, secret salt to harden the key derivation")
	}
	// The interface value must be a nil literal when audit is disabled —
	// a typed-nil *audit.Recorder would defeat the controller's nil check.
	var recorder extproc.AuditRecorder
	if r := e.AuditRecorder(); r != nil {
		recorder = r
	}
	ctrl, err := extproc.New(
		e.cfg,
		e.MaskUseCase(),
		e.SettingsService(),
		e.DemaskerProvider(),
		e.Store(),
		recorder,
	)
	if err != nil {
		panic(fmt.Errorf("create extproc controller: %w", err))
	}
	e.extproc = ctrl
	return e.extproc
}

// slogGRPCLogger adapts the default slog logger to the go-grpc-middleware
// logging interceptor (method/code/duration fields).
func slogGRPCLogger() grpclogging.Logger {
	return grpclogging.LoggerFunc(func(ctx context.Context, lvl grpclogging.Level, msg string, fields ...any) {
		slog.Log(ctx, slog.Level(lvl), msg, fields...)
	})
}

// GrpcController builds the gRPC management controller (GuardrailsApi) over
// the use-case layer, shared with the removed httpapi's dependency wiring.
func (e *Extproc) GrpcController(ctx context.Context) *api.Controller {
	if e.grpcController != nil {
		return e.grpcController
	}

	// The interface value must be a nil literal when audit is disabled — a
	// typed-nil *repository.Store would defeat the controller's nil check.
	var auditSvc api.AuditService
	var auditOriginals api.OriginalsDecrypter
	if e.cfg.Audit.Enabled {
		auditSvc = e.Store()
		// Shared codec opens encrypted audit originals on read (pass-through
		// for plaintext / when encryption is disabled).
		auditOriginals = e.storeCodec()
	}

	rulesUC := e.RulesUseCase()

	// Default scan scope: every declared data-type group plus CUSTOM, so a
	// bare scan exercises the whole ruleset.
	scanDataTypes := make([]models.DataType, 0, len(e.DataTypes())+1)
	for _, dt := range e.DataTypes() {
		scanDataTypes = append(scanDataTypes, models.DataType(dt.DataType))
	}
	scanDataTypes = append(scanDataTypes, models.DataTypeCUSTOM)
	scanUC := scanuc.New(scanuc.Deps{
		Production:       e.MaskUseCase(),
		FileRules:        e.loadFileRules(),
		DefaultDataTypes: scanDataTypes,
		KeywordPrefilter: e.cfg.Guardrails.KeywordPrefilterEnabled,
		ParallelMinBytes: e.cfg.Guardrails.MaskParallelMinBytes,
	})

	e.grpcController = api.NewController(api.Deps{
		Create:     rulesUC.Create(),
		Update:     rulesUC.Update(),
		Delete:     rulesUC.Delete(),
		SetEnabled: rulesUC.SetEnabled(),
		Get:        rulesUC.Get(),
		List:       rulesUC.List(),
		Scan:       scanUC,
		Settings:   e.SettingsService(),
		Audit:      auditSvc,
		Originals:  auditOriginals,
		DataTypes:  e.DataTypes(),
		BuildInfo: api.BuildInfo{
			Version:      version.Version,
			Commit:       version.Commit,
			Date:         version.Date,
			Topology:     "ext_proc",
			StoreBackend: e.cfg.Store.Backend,
		},
	})
	return e.grpcController
}

// GrpcServer returns the main gRPC server.
func (e *Extproc) GrpcServer(ctx context.Context) *grpc.Server {
	if e.grpcServer != nil {
		return e.grpcServer
	}

	keepAliveParams := keepalive.ServerParameters{
		MaxConnectionIdle:     10 * time.Hour,
		MaxConnectionAge:      24 * time.Hour,
		MaxConnectionAgeGrace: 5 * time.Minute,
		Time:                  60 * time.Second,
		Timeout:               1 * time.Second,
	}

	recoveryOpts := []recovery.Option{
		// Preserve the previous behavior: a recovered panic surfaces to the
		// client as codes.Internal, never as codes.Unknown.
		recovery.WithRecoveryHandlerContext(func(ctx context.Context, p any) error {
			logging.Error(ctx, "recovered from panic in gRPC handler", nil, "panic", p)
			return status.Error(codes.Internal, "internal error")
		}),
	}

	opts := []grpc.ServerOption{
		grpc.KeepaliveParams(keepAliveParams),
		grpc.ChainUnaryInterceptor(
			recovery.UnaryServerInterceptor(recoveryOpts...),
			grpclogging.UnaryServerInterceptor(slogGRPCLogger()),
			grpcprometheus.UnaryServerInterceptor,
			validator.UnaryServerInterceptor(),
		),
		grpc.ChainStreamInterceptor(
			recovery.StreamServerInterceptor(recoveryOpts...),
			grpclogging.StreamServerInterceptor(slogGRPCLogger()),
			grpcprometheus.StreamServerInterceptor,
			validator.StreamServerInterceptor(),
		),
	}

	if e.cfg.GrpcSecure {
		logging.Info(ctx, "gRPC is secure, using self-signed certificate")
		cert, err := tlsutils.CreateSelfSignedTLSCertificate()
		if err != nil {
			logging.Fatal(ctx, "Failed to create self signed certificate", err)
		}

		creds := credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}})
		opts = append(opts, grpc.Creds(creds))
	} else {
		logging.Info(ctx, "gRPC is insecure")
		opts = append(opts, grpc.Creds(insecure.NewCredentials()))
	}

	srv := grpc.NewServer(opts...)

	extprocv3.RegisterExternalProcessorServer(srv, e.ExtprocController())

	e.grpcServer = srv
	return e.grpcServer
}

// ManagementGrpcServer returns the management gRPC server (GuardrailsApi),
// separate from the ext_proc gRPC server on GRPCAddr/:9000 so Envoy's data
// path is never affected by management-API load or restarts.
func (e *Extproc) ManagementGrpcServer(ctx context.Context) *grpc.Server {
	if e.managementGrpcServer != nil {
		return e.managementGrpcServer
	}

	keepAliveParams := keepalive.ServerParameters{
		MaxConnectionIdle:     10 * time.Hour,
		MaxConnectionAge:      24 * time.Hour,
		MaxConnectionAgeGrace: 5 * time.Minute,
		Time:                  60 * time.Second,
		Timeout:               1 * time.Second,
	}

	pvValidator, err := protovalidate.New()
	if err != nil {
		logging.Fatal(ctx, "Failed to create protovalidate validator", err)
	}

	opts := []grpc.ServerOption{
		grpc.KeepaliveParams(keepAliveParams),
		grpc.ChainUnaryInterceptor(
			recovery.UnaryServerInterceptor(),
			grpclogging.UnaryServerInterceptor(slogGRPCLogger()),
			grpcprometheus.UnaryServerInterceptor,
			protovalidate_middleware.UnaryServerInterceptor(pvValidator),
		),
		grpc.ChainStreamInterceptor(
			recovery.StreamServerInterceptor(),
			grpclogging.StreamServerInterceptor(slogGRPCLogger()),
			grpcprometheus.StreamServerInterceptor,
			protovalidate_middleware.StreamServerInterceptor(pvValidator),
		),
	}

	if e.cfg.GrpcSecure {
		logging.Info(ctx, "management gRPC is secure, using self-signed certificate")
		cert, err := tlsutils.CreateSelfSignedTLSCertificate()
		if err != nil {
			logging.Fatal(ctx, "Failed to create self signed certificate", err)
		}
		creds := credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}})
		opts = append(opts, grpc.Creds(creds))
	} else {
		opts = append(opts, grpc.Creds(insecure.NewCredentials()))
	}

	srv := grpc.NewServer(opts...)
	servicev1.RegisterGuardrailsApiServer(srv, e.GrpcController(ctx))
	reflection.Register(srv)

	e.managementGrpcServer = srv
	return e.managementGrpcServer
}

// HealthServer returns the health gRPC server.
func (e *Extproc) HealthServer() *grpc.Server {
	if e.healthServer != nil {
		return e.healthServer
	}
	srv := grpc.NewServer()
	grpchealth.RegisterHealthServer(srv, &healthChecker{})
	e.healthServer = srv
	return e.healthServer
}

// APIServer returns the management REST server: a grpc-gateway reverse proxy
// to the gRPC listener (GuardrailsApi), plus the REST-only /v1/health and
// /v1/metrics/summary endpoints. Returns nil when the API is disabled (empty
// API_ADDR).
func (e *Extproc) APIServer(ctx context.Context) *http.Server {
	if e.apiServer != nil {
		return e.apiServer
	}
	if e.cfg.API.Addr == "" {
		return nil
	}
	// The configuration API is unauthenticated: it exposes mutating endpoints
	// (rules, settings) with no token check, so it must be protected at the
	// network layer (cluster-internal only, never public ingress).
	logging.Warn(ctx, "Configuration API is unauthenticated; protect it at the network level (no public ingress)")

	mux := runtime.NewServeMux(
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{
			// UseProtoNames keeps snake_case field names and UseEnumNumbers keeps
			// data_type as numbers on the wire.
			MarshalOptions:   protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true, EmitUnpopulated: true},
			UnmarshalOptions: protojson.UnmarshalOptions{},
		}),
	)

	dialCreds := grpc.WithTransportCredentials(insecure.NewCredentials())
	if e.cfg.GrpcSecure {
		//nolint:gosec // loopback dial to our own self-signed listener
		dialCreds = grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true}))
	}
	if err := servicev1.RegisterGuardrailsApiHandlerFromEndpoint(
		ctx, mux, dialTarget(e.cfg.API.GRPCAddr), []grpc.DialOption{dialCreds},
	); err != nil {
		logging.Fatal(ctx, "Failed to register management gateway", err)
	}

	// REST-only endpoints not modeled in the proto contract.
	if err := mux.HandlePath("GET", "/v1/health", e.handleHealth); err != nil {
		logging.Fatal(ctx, "Failed to register /v1/health", err)
	}
	if err := mux.HandlePath("GET", "/v1/metrics/summary", e.handleMetricsSummary); err != nil {
		logging.Fatal(ctx, "Failed to register /v1/metrics/summary", err)
	}

	e.apiServer = &http.Server{
		Addr:              e.cfg.API.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	return e.apiServer
}

// handleHealth is a lightweight liveness signal on the management port. It
// also echoes the live mode and store backend so a console can render
// status.
func (e *Extproc) handleHealth(w http.ResponseWriter, _ *http.Request, _ map[string]string) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":        "ok",
		"mode":          string(e.SettingsService().Global().Mode),
		"store_backend": e.cfg.Store.Backend,
	})
}

// dialTarget makes an in-process dial target from a listen address: a
// wildcard/empty host is rewritten to loopback so the gateway can reach the
// local gRPC listener.
func dialTarget(addr string) string {
	switch {
	case strings.HasPrefix(addr, ":"):
		return "127.0.0.1" + addr
	case strings.HasPrefix(addr, "0.0.0.0:"):
		return "127.0.0.1" + strings.TrimPrefix(addr, "0.0.0.0")
	default:
		return addr
	}
}

// MetricsServer returns the Prometheus metrics HTTP server.
func (e *Extproc) MetricsServer() *http.Server {
	if e.metricsServer != nil {
		return e.metricsServer
	}
	e.metricsServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", e.cfg.MetricsPort),
		Handler: promhttp.Handler(),
	}
	return e.metricsServer
}

// Start brings up all servers and registers a graceful shutdown handler.
func (e *Extproc) Start(ctx context.Context) error {
	// Initialise settings and merge stored custom rules before serving.
	// Both are fail-open: env defaults / file rules serve until the store
	// heals via the refresh tickers.
	if err := e.SettingsService().Load(ctx); err != nil {
		logging.Error(ctx, "Failed to load settings from store, using env defaults", err)
	}
	if err := e.RulesReloader().Reload(ctx); err != nil {
		logging.Error(ctx, "Failed to load custom rules from store, serving file rules only", err)
	}

	if e.cfg.Audit.Enabled {
		logging.Info(ctx, "Masking audit trail enabled",
			"store_masked_texts", e.cfg.Audit.StoreMaskedTexts,
			"store_masked_response_texts", e.cfg.Audit.StoreMaskedResponseTexts,
			"store_original_texts", e.cfg.Audit.StoreOriginalTexts,
			"retention", e.cfg.Audit.Retention.String())
	}

	grpcAddr := e.cfg.GRPCAddr
	grpcLis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		return fmt.Errorf("listen gRPC on %s: %w", grpcAddr, err)
	}

	go func() {
		logging.Info(ctx, "Starting gRPC server", "addr", grpcAddr)
		if err := e.GrpcServer(ctx).Serve(grpcLis); err != nil {
			logging.Error(ctx, "gRPC server error", err)
		}
	}()

	healthAddr := fmt.Sprintf(":%d", e.cfg.HealthPort)
	healthLis, err := net.Listen("tcp", healthAddr)
	if err != nil {
		return fmt.Errorf("listen health on %s: %w", healthAddr, err)
	}

	go func() {
		logging.Info(ctx, "Starting health server", "addr", healthAddr)
		if err := e.HealthServer().Serve(healthLis); err != nil {
			logging.Error(ctx, "health server error", err)
		}
	}()

	go func() {
		logging.Info(ctx, "Starting metrics server", "port", e.cfg.MetricsPort)
		if err := e.MetricsServer().ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logging.Error(ctx, "metrics server error", err)
		}
	}()

	var mgmtGrpcLis net.Listener
	if e.cfg.API.Addr != "" {
		mgmtGrpcLis, err = net.Listen("tcp", e.cfg.API.GRPCAddr)
		if err != nil {
			return fmt.Errorf("listen management gRPC on %s: %w", e.cfg.API.GRPCAddr, err)
		}
		go func() {
			logging.Info(ctx, "Starting management gRPC server", "addr", e.cfg.API.GRPCAddr)
			if err := e.ManagementGrpcServer(ctx).Serve(mgmtGrpcLis); err != nil {
				logging.Error(ctx, "management gRPC server error", err)
			}
		}()
	}

	if apiSrv := e.APIServer(ctx); apiSrv != nil {
		go func() {
			logging.Info(ctx, "Starting configuration API server", "addr", apiSrv.Addr)
			if err := apiSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logging.Error(ctx, "configuration API server error", err)
			}
		}()
	}

	// Background refresh converges replicas on API changes when the store
	// backend is shared.
	refreshCtx, cancelRefresh := context.WithCancel(context.Background())
	go e.SettingsService().RunRefresh(refreshCtx, e.cfg.Guardrails.SettingsRefreshInterval)
	go e.RulesReloader().RunRefresh(refreshCtx, e.cfg.Guardrails.RulesRefreshInterval)

	e.stop = func() error {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		cancelRefresh()

		var apiErr error
		if e.apiServer != nil {
			apiErr = e.apiServer.Shutdown(shutdownCtx)
		}

		metricsErr := e.MetricsServer().Shutdown(shutdownCtx)
		e.HealthServer().GracefulStop()

		// GracefulStop blocks until every in-flight stream ends; a long-lived
		// SSE response would otherwise stall shutdown until SIGKILL. Bound it
		// by shutdownCtx, falling back to a forced Stop.
		grpcSrv := e.GrpcServer(context.Background())
		grpcDone := make(chan struct{})
		go func() {
			grpcSrv.GracefulStop()
			close(grpcDone)
		}()
		select {
		case <-grpcDone:
		case <-shutdownCtx.Done():
			grpcSrv.Stop()
			<-grpcDone
		}

		if e.managementGrpcServer != nil {
			mgmtDone := make(chan struct{})
			go func() {
				e.managementGrpcServer.GracefulStop()
				close(mgmtDone)
			}()
			select {
			case <-mgmtDone:
			case <-shutdownCtx.Done():
				e.managementGrpcServer.Stop()
				<-mgmtDone
			}
		}

		// Drain the audit recorder before closing the store so in-flight
		// (detached) writes are not lost. Bounded by shutdownCtx.
		if e.auditRecorder != nil {
			e.auditRecorder.Drain(shutdownCtx)
		}

		var storeErr error
		if e.store != nil {
			storeErr = e.store.Close()
		}

		return multierr.Combine(apiErr, metricsErr, storeErr)
	}

	health.SetLiveness(true)
	health.SetReadiness(true)

	logging.Info(ctx, "All servers started")
	return nil
}

// Stop gracefully shuts down all servers, background tickers and the repository.
// It is a no-op when Start was never called.
func (e *Extproc) Stop() error {
	if e.stop == nil {
		return nil
	}
	return e.stop()
}
