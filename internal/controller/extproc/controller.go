package extproc

import (
	"context"
	"fmt"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/config"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/logging"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/models"
	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/repository"
)

// Controller implements the Envoy External Processor gRPC service.
type Controller struct {
	extprocv3.UnimplementedExternalProcessorServer
	cfg              *config.Config
	masker           Masker
	settingsProvider SettingsProvider
	demaskerProvider DemaskerProvider
	stateStore       repository.MaskingStateStore
	audit            AuditRecorder // nil disables the audit trail
	pathResolver     *models.PathResolver
}

// New creates a new Controller. audit may be nil (audit trail disabled).
// The path map (GUARDRAILS_PATHS) is validated here; an invalid map is a
// boot-time error.
func New(
	cfg *config.Config,
	masker Masker,
	settingsProvider SettingsProvider,
	demaskerProvider DemaskerProvider,
	stateStore repository.MaskingStateStore,
	audit AuditRecorder,
) (*Controller, error) {
	resolver, err := models.NewPathResolver(cfg.Guardrails.Paths)
	if err != nil {
		return nil, fmt.Errorf("build path resolver: %w", err)
	}
	return &Controller{
		cfg:              cfg,
		masker:           masker,
		settingsProvider: settingsProvider,
		demaskerProvider: demaskerProvider,
		stateStore:       stateStore,
		audit:            audit,
		pathResolver:     resolver,
	}, nil
}

// Process handles the Envoy external processing requests
func (c *Controller) Process(srv extprocv3.ExternalProcessor_ProcessServer) error {
	ctx := srv.Context()
	proc := newRequestProcessor(
		c.cfg,
		c.masker,
		c.settingsProvider,
		c.demaskerProvider,
		c.stateStore,
		c.audit,
		c.pathResolver,
	)

	defer func() {
		logging.Debug(ctx, "closing request processor in defered statement", proc.LogOpts()...)
		err := proc.Close()
		if err != nil {
			logging.Error(ctx, "failed to close request processor", err, proc.LogOpts()...)
		}
	}()

	// Process incoming requests
	for {
		req, err := srv.Recv()
		if err != nil {
			// This error occurs very frequently when clients disconnect normally.
			// It's expected behavior and doesn't indicate an actual problem.
			// Downgrade from Error to Debug level to reduce log noise.
			logging.Debug(ctx, "client disconnected or connection closed", "error", err)
			return nil
		}

		resp, err := c.handleRequest(ctx, &proc, req)
		if err != nil {
			return status.Errorf(codes.Internal, "failed to handle request: %v", err)
		}

		// If the processor says to skip all steps, close the stream
		// because no more messages are needed.
		if proc.ShouldSkip(StepAll) {
			return nil
		}

		//	Do not send any response in observability mode
		//  or if response is nil (valid for FULL_DUPLEX_STREAMED response body send mode)
		if resp == nil || req.ObservabilityMode {
			continue
		}

		if err := srv.Send(resp); err != nil {
			return status.Errorf(codes.Unknown, "failed to send response back to Envoy: %v", err)
		}
	}
}

func (c *Controller) handleRequest(ctx context.Context, proc *requestProcessor, req *extprocv3.ProcessingRequest) (*extprocv3.ProcessingResponse, error) {
	switch r := req.Request.(type) {
	case *extprocv3.ProcessingRequest_RequestHeaders:
		if proc.ShouldSkip(StepRequestHeaders) {
			logging.Debug(ctx, "Skipping RequestHeaders message, because ShouldSkip")
			if req.ObservabilityMode {
				return nil, nil
			}
			return &extprocv3.ProcessingResponse{
				Response: &extprocv3.ProcessingResponse_RequestHeaders{
					RequestHeaders: &extprocv3.HeadersResponse{},
				},
			}, nil
		}
		return proc.HandleRequestHeaders(ctx, r.RequestHeaders)

	case *extprocv3.ProcessingRequest_RequestBody:
		if proc.ShouldSkip(StepRequestBody) {
			logging.Debug(ctx, "Skipping RequestBody message, because ShouldSkip")
			if req.ObservabilityMode {
				return nil, nil
			}
			return &extprocv3.ProcessingResponse{
				Response: &extprocv3.ProcessingResponse_RequestBody{
					RequestBody: &extprocv3.BodyResponse{},
				},
			}, nil
		}
		return proc.HandleRequestBody(ctx, r.RequestBody)

	case *extprocv3.ProcessingRequest_RequestTrailers:
		if proc.ShouldSkip(StepRequestTrailers) {
			logging.Debug(ctx, "Skipping RequestTrailers message, because ShouldSkip")
			if req.ObservabilityMode {
				return nil, nil
			}
			return &extprocv3.ProcessingResponse{
				Response: &extprocv3.ProcessingResponse_RequestTrailers{
					RequestTrailers: &extprocv3.TrailersResponse{},
				},
			}, nil
		}
		return proc.HandleRequestTrailers(ctx, r.RequestTrailers)

	case *extprocv3.ProcessingRequest_ResponseHeaders:
		if proc.ShouldSkip(StepResponseHeaders) {
			logging.Debug(ctx, "Skipping ResponseHeaders message, because ShouldSkip")
			if req.ObservabilityMode {
				return nil, nil
			}
			return &extprocv3.ProcessingResponse{
				Response: &extprocv3.ProcessingResponse_ResponseHeaders{
					ResponseHeaders: &extprocv3.HeadersResponse{},
				},
			}, nil
		}
		return proc.HandleResponseHeaders(ctx, r.ResponseHeaders)

	case *extprocv3.ProcessingRequest_ResponseBody:
		if proc.ShouldSkip(StepResponseBody) {
			logging.Debug(ctx, "Skipping ResponseBody message, because ShouldSkip")
			if req.ObservabilityMode {
				return nil, nil
			}
			return &extprocv3.ProcessingResponse{
				Response: &extprocv3.ProcessingResponse_ResponseBody{
					ResponseBody: &extprocv3.BodyResponse{},
				},
			}, nil
		}
		return proc.HandleResponseBody(ctx, r.ResponseBody)

	case *extprocv3.ProcessingRequest_ResponseTrailers:
		if proc.ShouldSkip(StepResponseTrailers) {
			logging.Debug(ctx, "Skipping ResponseTrailers message, because ShouldSkip")
			if req.ObservabilityMode {
				return nil, nil
			}
			return &extprocv3.ProcessingResponse{
				Response: &extprocv3.ProcessingResponse_ResponseTrailers{
					ResponseTrailers: &extprocv3.TrailersResponse{},
				},
			}, nil
		}
		return proc.HandleResponseTrailers(ctx, r.ResponseTrailers)

	default:
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_RequestHeaders{
				RequestHeaders: &extprocv3.HeadersResponse{
					Response: &extprocv3.CommonResponse{
						Status: extprocv3.CommonResponse_CONTINUE,
					},
				},
			},
		}, nil
	}
}
