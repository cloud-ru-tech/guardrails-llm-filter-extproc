package app

import (
	"context"

	grpchealth "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/cloud-ru-tech/guardrails-llm-filter-extproc/internal/health"
)

type healthChecker struct {
	grpchealth.UnimplementedHealthServer
}

func (h *healthChecker) Check(_ context.Context, _ *grpchealth.HealthCheckRequest) (*grpchealth.HealthCheckResponse, error) {
	if health.GetLiveness() {
		return &grpchealth.HealthCheckResponse{Status: grpchealth.HealthCheckResponse_SERVING}, nil
	}
	return &grpchealth.HealthCheckResponse{Status: grpchealth.HealthCheckResponse_NOT_SERVING}, nil
}

func (h *healthChecker) Watch(req *grpchealth.HealthCheckRequest, stream grpchealth.Health_WatchServer) error {
	resp := &grpchealth.HealthCheckResponse{}
	if health.GetReadiness() {
		resp.Status = grpchealth.HealthCheckResponse_SERVING
	} else {
		resp.Status = grpchealth.HealthCheckResponse_NOT_SERVING
	}
	return stream.Send(resp)
}
