package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openzot/openzot/internal/imaging"
)

func TestChatMessageMarshalsAsAStringWhenItHasNoImages(t *testing.T) {
	encoded, err := json.Marshal(ChatMessage{Role: RoleUser, Content: "plain"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded struct {
		Content json.RawMessage `json:"content"`
	}

	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if string(decoded.Content) != `"plain"` {
		t.Errorf("content = %s, want the scalar every provider accepts", decoded.Content)
	}
}

func TestChatMessageContentArrayWrapsPlainText(t *testing.T) {
	encoded, err := json.Marshal(ChatMessage{Role: RoleUser, Content: "hello", ContentArray: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded struct {
		Content []map[string]any `json:"content"`
	}

	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("content is not an array of parts: %v", err)
	}

	if len(decoded.Content) != 1 || decoded.Content[0]["type"] != "text" || decoded.Content[0]["text"] != "hello" {
		t.Errorf("content = %v, want a single text part", decoded.Content)
	}
}

// llama.cpp rejects a missing content key, and array-only templates reject a
// string, so an empty message must still carry [].
func TestChatMessageContentArraySendsEmptyArrayForNoContent(t *testing.T) {
	for _, role := range []string{RoleUser, RoleSystem, RoleTool, RoleAssistant} {
		encoded, err := json.Marshal(ChatMessage{Role: role, ContentArray: true})
		if err != nil {
			t.Fatalf("marshal %s: %v", role, err)
		}

		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("unmarshal %s: %v", role, err)
		}

		if content, ok := decoded["content"]; !ok || string(content) != `[]` {
			t.Errorf("%s content = %s, ok=%v, want an empty array present", role, content, ok)
		}
	}
}

func TestChatMessagePromotesContentToPartsWhenItCarriesImages(t *testing.T) {
	message := ChatMessage{
		Role:    RoleUser,
		Content: "look at this",
		Images:  []imaging.Image{imaging.NewImage([]byte{9, 9}, "image/png", 20, 10)},
	}

	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded struct {
		Content []map[string]any `json:"content"`
	}

	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("content is not an array of parts: %v", err)
	}

	if len(decoded.Content) != 2 {
		t.Fatalf("got %d parts, want the text and the image", len(decoded.Content))
	}

	if decoded.Content[0]["type"] != "text" || decoded.Content[0]["text"] != "look at this" {
		t.Errorf("first part = %v, want the message text", decoded.Content[0])
	}

	if decoded.Content[1]["type"] != "image_url" {
		t.Fatalf("second part = %v, want an image", decoded.Content[1])
	}

	url, _ := decoded.Content[1]["image_url"].(map[string]any)

	if !strings.HasPrefix(url["url"].(string), "data:image/png;base64,") {
		t.Errorf("image url = %v, want a data URL", url["url"])
	}

	if strings.Contains(string(encoded), `"images"`) {
		t.Error("images must reach the wire as content parts, not as a field of their own")
	}
}

func TestChatMessageCarriesTheDetailHintWhenSet(t *testing.T) {
	image := imaging.NewImage([]byte{1}, "image/png", 10, 10)
	image.Detail = "low"

	encoded, err := json.Marshal(ChatMessage{Role: RoleUser, Images: []imaging.Image{image}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if !strings.Contains(string(encoded), `"detail":"low"`) {
		t.Errorf("detail hint did not reach the wire: %s", encoded)
	}
}

func TestChatMessageSkipsAnImageWithNoBytes(t *testing.T) {
	message := ChatMessage{
		Role:    RoleUser,
		Content: "the screenshot described below",
		Images:  []imaging.Image{{MediaType: "image/png", Digest: "sha256:gone"}},
	}

	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded struct {
		Content json.RawMessage `json:"content"`
	}

	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if string(decoded.Content) != `"the screenshot described below"` {
		t.Errorf("content = %s; an image whose blob is missing must leave the text alone rather than send an empty part", decoded.Content)
	}
}
