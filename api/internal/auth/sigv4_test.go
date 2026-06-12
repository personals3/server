package auth

import (
	"bytes"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// Signed-payload verification buffers the whole body; these tests pin the
// size cap that keeps a large signed PUT from ballooning API memory.

func TestVerifySigV4RejectsOversizedSignedPayload(t *testing.T) {
	// Content-Length fast path: rejected before any buffering.
	req := httptest.NewRequest("PUT", "/bucket/key", strings.NewReader("ignored"))
	req.Header.Set("X-Amz-Content-Sha256", strings.Repeat("a", 64))
	req.ContentLength = maxSignedPayloadBytes + 1

	err := VerifySigV4(req, &SigV4Components{}, "secret")
	if !errors.Is(err, ErrSignedPayloadTooLarge) {
		t.Fatalf("want ErrSignedPayloadTooLarge, got %v", err)
	}
}

func TestHashRequestBodyRejectsOversizedChunkedBody(t *testing.T) {
	// No Content-Length (chunked): the LimitReader catches it instead.
	big := bytes.Repeat([]byte("x"), maxSignedPayloadBytes+1)
	req := httptest.NewRequest("PUT", "/bucket/key", io.MultiReader(bytes.NewReader(big)))
	if req.ContentLength != -1 {
		t.Fatalf("test setup: want unknown ContentLength, got %d", req.ContentLength)
	}

	_, err := hashRequestBody(req)
	if !errors.Is(err, ErrSignedPayloadTooLarge) {
		t.Fatalf("want ErrSignedPayloadTooLarge, got %v", err)
	}
}

func TestHashRequestBodyStillHashesAndRewindsSmallBodies(t *testing.T) {
	req := httptest.NewRequest("PUT", "/bucket/key", strings.NewReader("hello"))

	got, err := hashRequestBody(req)
	if err != nil {
		t.Fatalf("hashRequestBody: %v", err)
	}
	// sha256("hello")
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Errorf("hash: want %s, got %s", want, got)
	}
	// Body must be replaced so the handler can still read it.
	body, _ := io.ReadAll(req.Body)
	if string(body) != "hello" {
		t.Errorf("body not rewound: %q", body)
	}
}
