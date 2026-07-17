package web

import (
	"encoding/binary"
	"testing"
)

func grpcTrailerFrame(value string) []byte {
	payload := []byte("grpc-status: " + value + "\n")
	frame := make([]byte, 5+len(payload))
	frame[0] = 0x80
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

func TestGrpcWebStatusAllowsEmptySuccessfulResponse(t *testing.T) {
	if err := grpcWebStatus(grpcTrailerFrame("0")); err != nil {
		t.Fatalf("expected empty successful response to pass: %v", err)
	}
}

func TestGrpcWebStatusReportsTrailerError(t *testing.T) {
	if err := grpcWebStatus(grpcTrailerFrame("7")); err == nil {
		t.Fatal("expected non-zero grpc status to fail")
	}
}
