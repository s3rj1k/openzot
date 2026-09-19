package imaging

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestNewImageDescribesTheBytesItIsGiven(t *testing.T) {
	data := []byte("pretend png")

	image := NewImage(data, "image/png", 800, 600)

	if image.Size != len(data) {
		t.Errorf("Size = %d, want %d", image.Size, len(data))
	}

	if !strings.HasPrefix(image.Digest, "sha256:") {
		t.Errorf("Digest = %q, want a sha256 content address", image.Digest)
	}

	if image.Digest != Digest(data) {
		t.Error("an image's digest must be the digest of its own bytes")
	}

	if image.Tokens <= 0 {
		t.Error("an image must carry a token estimate, or the budget is blind to it")
	}
}

func TestDigestChangesWithTheBytes(t *testing.T) {
	if Digest([]byte("a")) == Digest([]byte("b")) {
		t.Fatal("different bytes must not share a content address")
	}

	if Digest([]byte("same")) != Digest([]byte("same")) {
		t.Error("the same bytes must address the same blob, so a repeated image is stored once")
	}
}

func TestExtensionNamesTheBlobByType(t *testing.T) {
	cases := map[string]string{
		"image/png":                "png",
		"image/jpeg":               "jpg",
		"image/gif":                "gif",
		"image/webp":               "webp",
		"application/octet-stream": "bin",
	}

	for mediaType, want := range cases {
		if got := (Image{MediaType: mediaType}).Extension(); got != want {
			t.Errorf("%s extension = %q, want %q", mediaType, got, want)
		}
	}
}

func TestRawReturnsTheBytesWhetherHeldOrInline(t *testing.T) {
	held := NewImage([]byte{1, 2, 3}, "image/png", 10, 10)

	if !bytes.Equal(held.Raw(), []byte{1, 2, 3}) {
		t.Errorf("Raw = %v, want the bytes held in memory", held.Raw())
	}

	// a record read back from a log carries base64 rather than bytes
	stored := Image{MediaType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte{1, 2, 3})}

	if !bytes.Equal(stored.Raw(), []byte{1, 2, 3}) {
		t.Errorf("Raw = %v, want an inline image to decode to the same bytes", stored.Raw())
	}

	if (Image{Data: "not base64!"}).Raw() != nil {
		t.Error("a corrupt inline payload must yield nothing rather than garbage")
	}
}

func TestReadyDistinguishesAnImageWithNoBytes(t *testing.T) {
	if (Image{MediaType: "image/png"}).Ready() {
		t.Error("an image whose blob went missing must not report itself ready to send")
	}

	if !(Image{Bytes: []byte("x")}).Ready() {
		t.Error("an image with bytes is ready")
	}

	if !(Image{Data: "eA=="}).Ready() {
		t.Error("an inline image is ready")
	}
}

func TestEstimateImageTokensGrowsWithArea(t *testing.T) {
	small := EstimateImageTokens(100, 100)
	large := EstimateImageTokens(2000, 2000)

	if small <= 0 {
		t.Fatalf("a measurable image must cost something, got %d", small)
	}

	if large <= small {
		t.Errorf("a larger image must estimate higher: %d vs %d", large, small)
	}

	if unmeasurable := EstimateImageTokens(0, 0); unmeasurable <= 0 {
		t.Error("an unmeasurable image must not be estimated at nothing - the budget would ignore it")
	}
}
