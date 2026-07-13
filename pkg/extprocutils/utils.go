package extprocutils

import (
	"strconv"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	ext_procv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
)

const (
	ContentLengthHeader = "content-length"
	ContentTypeHeader   = "content-type"
	ContentTypeText     = "text/plain; charset=utf-8"
)

func GetHeader(target *corev3.HeaderMap, key string) (string, bool) {
	for _, header := range target.GetHeaders() {
		if header.Key == strings.ToLower(key) {
			if len(header.RawValue) > 0 {
				return string(header.RawValue), true
			} else {
				return header.Value, true
			}
		}
	}

	return "", false
}

func HeadersToMap(target *corev3.HeaderMap) map[string]string {
	headersMap := make(map[string]string, len(target.GetHeaders()))

	for _, header := range target.GetHeaders() {
		key := strings.ToLower(header.Key)

		if len(header.RawValue) > 0 {
			headersMap[key] = string(header.RawValue)
		} else {
			headersMap[key] = header.Value
		}
	}

	return headersMap
}

func ImmediateResponse(code typev3.StatusCode) *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extprocv3.ImmediateResponse{
				Status: &typev3.HttpStatus{Code: code},
			},
		},
	}
}

func ModeOverrideSkipping() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{},
		},
		ModeOverride: &ext_procv3.ProcessingMode{
			RequestHeaderMode:   ext_procv3.ProcessingMode_SKIP,
			RequestBodyMode:     ext_procv3.ProcessingMode_NONE,
			RequestTrailerMode:  ext_procv3.ProcessingMode_SKIP,
			ResponseHeaderMode:  ext_procv3.ProcessingMode_SKIP,
			ResponseBodyMode:    ext_procv3.ProcessingMode_NONE,
			ResponseTrailerMode: ext_procv3.ProcessingMode_SKIP,
		},
	}
}

func BodyMutation(newBody []byte) *extprocv3.CommonResponse {
	return &extprocv3.CommonResponse{
		BodyMutation: &extprocv3.BodyMutation{
			Mutation: &extprocv3.BodyMutation_Body{
				Body: newBody,
			},
		},
		HeaderMutation: &extprocv3.HeaderMutation{
			SetHeaders: []*corev3.HeaderValueOption{
				{
					Header: &corev3.HeaderValue{
						Key:      ContentLengthHeader,
						RawValue: []byte(strconv.Itoa(len(newBody))),
					},
				},
			},
		},
	}
}

// StreamedBodyMutation builds a ProcessingResponse that replaces the current
// streamed body chunk with the provided bytes.
// Must be used in FULL_DUPLEX_STREAMED mode instead of BodyMutation_Body.
func StreamedBodyMutation(body []byte, endOfStream bool) *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ResponseBody{
			ResponseBody: &extprocv3.BodyResponse{
				Response: &extprocv3.CommonResponse{
					BodyMutation: &extprocv3.BodyMutation{
						Mutation: &extprocv3.BodyMutation_StreamedResponse{
							StreamedResponse: &extprocv3.StreamedBodyResponse{
								Body:        body,
								EndOfStream: endOfStream,
							},
						},
					},
				},
			},
		},
	}
}

func NewHeader(key, value string) *corev3.HeaderValueOption {
	return &corev3.HeaderValueOption{
		Header: &corev3.HeaderValue{
			Key:      key,
			RawValue: []byte(value),
		},
	}
}

func HeadersMutation(headers ...*corev3.HeaderValueOption) *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extprocv3.HeadersResponse{
				Response: &extprocv3.CommonResponse{
					HeaderMutation: &extprocv3.HeaderMutation{
						SetHeaders: headers,
					},
				},
			},
		},
	}
}
