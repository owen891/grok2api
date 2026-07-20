package web

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestDecodeImageBlobDataURL(t *testing.T) {
	want := []byte("fake-png-bytes")
	value := "data:image/png;base64," + base64.StdEncoding.EncodeToString(want)
	got, err := decodeImageBlob(value)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded bytes = %q, want %q", got, want)
	}
}

func TestAbsoluteAssetURLPreservesDataURL(t *testing.T) {
	value := "data:image/png;base64,ZmFrZQ=="
	if got := absoluteAssetURL(value); got != value {
		t.Fatalf("absoluteAssetURL = %q, want %q", got, value)
	}
}

func TestParseUpstreamFramePreservesDataImageURL(t *testing.T) {
	parsed := &parsedChat{}
	frame := []byte(`{"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"data:image/png;base64,ZmFrZQ==","progress":100,"isFinal":true}}}}`)
	kind, delta, err := parseUpstreamFrame(frame, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "image" || delta != "data:image/png;base64,ZmFrZQ==" {
		t.Fatalf("kind=%q delta=%q", kind, delta)
	}
}
