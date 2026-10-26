package grpcx

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"
)

// Error returns a status error with a google.rpc.ErrorInfo detail: a stable, machine-readable
// reason (e.g., DATABASE_UNAVAILABLE) in the service's domain (e.g., "order.orderflow"), so
// callers decide by the reason instead of parsing the message.
func Error(code codes.Code, domain, reason, message string) error {
	return withDetails(status.New(code, message), &errdetails.ErrorInfo{Reason: reason, Domain: domain})
}

// FieldError returns INVALID_ARGUMENT for an invalid request field, with the ErrorInfo and a
// google.rpc.BadRequest field violation. The message is "field: description".
func FieldError(domain, reason, field, description string) error {
	return withDetails(status.New(codes.InvalidArgument, field+": "+description),
		&errdetails.ErrorInfo{Reason: reason, Domain: domain},
		&errdetails.BadRequest{FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: field, Description: description}}},
	)
}

func withDetails(st *status.Status, details ...protoadapt.MessageV1) error {
	withDetails, err := st.WithDetails(details...)
	if err != nil {
		return st.Err() // the details could not be encoded; the code and message still go
	}
	return withDetails.Err()
}

// Reason returns the ErrorInfo reason of a status error, or "" if there is none (e.g., the
// error was produced by gRPC itself: an unreachable server, an expired deadline).
func Reason(err error) string {
	st, ok := status.FromError(err)
	if !ok {
		return ""
	}
	for _, detail := range st.Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok {
			return info.GetReason()
		}
	}
	return ""
}

// FieldViolation returns the first invalid field of a status error and its description.
func FieldViolation(err error) (field, description string, ok bool) {
	st, isStatus := status.FromError(err)
	if !isStatus {
		return "", "", false
	}
	for _, detail := range st.Details() {
		if br, isBadRequest := detail.(*errdetails.BadRequest); isBadRequest && len(br.GetFieldViolations()) > 0 {
			v := br.GetFieldViolations()[0]
			return v.GetField(), v.GetDescription(), true
		}
	}
	return "", "", false
}
