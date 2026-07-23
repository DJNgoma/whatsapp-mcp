package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestIsLoopbackHost(t *testing.T) {
	for _, host := range []string{"127.0.0.1:8741", "localhost:8741", "localhost.", "[::1]:8741"} {
		if !isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = false, want true", host)
		}
	}
	for _, host := range []string{"", "example.com:8741", "127.0.0.2.example:8741"} {
		if isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = true, want false", host)
		}
	}
}

func TestProtectLoopbackAPIRejectsUntrustedAuthority(t *testing.T) {
	reached := false
	handler := protectLoopbackAPI(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}), 1024)

	request := httptest.NewRequest(http.MethodGet, "http://example.invalid/api/health", nil)
	request.Host = "example.invalid:8741"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
	if reached {
		t.Fatal("protected handler was reached for a non-loopback Host")
	}
}

func TestProtectLoopbackAPIRejectsBrowserOrigin(t *testing.T) {
	reached := false
	handler := protectLoopbackAPI(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}), 1024)

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8741/api/health", nil)
	request.Host = "127.0.0.1:8741"
	request.Header.Set("Origin", "https://example.invalid")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
	}
	if reached {
		t.Fatal("protected handler was reached for a browser Origin")
	}
}

func TestProtectLoopbackAPILimitsRequestBodies(t *testing.T) {
	handler := protectLoopbackAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), 8)

	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8741/api/test", strings.NewReader("123456789"))
	request.Host = "127.0.0.1:8741"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusRequestEntityTooLarge)
	}

	request = httptest.NewRequest(http.MethodPost, "http://localhost:8741/api/test", strings.NewReader("1234"))
	request.Host = "localhost:8741"
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("bounded request status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestHealthResponsePreservesConfiguredInstanceID(t *testing.T) {
	t.Setenv("WHATSAPP_BRIDGE_INSTANCE_ID", "work-profile:8742")
	encoded, err := json.Marshal(HealthResponse{InstanceID: configuredBridgeInstanceID()})
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err = json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	if got := response["instance_id"]; got != "work-profile:8742" {
		t.Fatalf("instance_id = %#v, want work-profile:8742", got)
	}

	t.Setenv("WHATSAPP_BRIDGE_INSTANCE_ID", "")
	encoded, err = json.Marshal(HealthResponse{InstanceID: configuredBridgeInstanceID()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "instance_id") {
		t.Fatalf("empty optional instance_id was serialized: %s", encoded)
	}
}

func TestConfiguredBridgeLimits(t *testing.T) {
	t.Setenv("WHATSAPP_BRIDGE_MAX_REQUEST_BODY_BYTES", "2048")
	t.Setenv("WHATSAPP_BRIDGE_MAX_MEDIA_DOWNLOAD_BYTES", "4096")
	if got := configuredMaxBridgeRequestBodyBytes(); got != 2048 {
		t.Fatalf("configured request limit = %d, want 2048", got)
	}
	if got := configuredMaxMediaDownloadBytes(); got != 4096 {
		t.Fatalf("configured media limit = %d, want 4096", got)
	}

	t.Setenv("WHATSAPP_BRIDGE_MAX_REQUEST_BODY_BYTES", "invalid")
	t.Setenv("WHATSAPP_BRIDGE_MAX_MEDIA_DOWNLOAD_BYTES", "0")
	if got := configuredMaxBridgeRequestBodyBytes(); got != defaultMaxBridgeRequestBodyBytes {
		t.Fatalf("invalid request limit fallback = %d, want %d", got, defaultMaxBridgeRequestBodyBytes)
	}
	if got := configuredMaxMediaDownloadBytes(); got != defaultMaxMediaDownloadBytes {
		t.Fatalf("invalid media limit fallback = %d, want %d", got, defaultMaxMediaDownloadBytes)
	}
}

func TestSecureStorePermissionsTightensExistingState(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "store")
	mediaDir := filepath.Join(storeDir, "chat")
	if err := os.MkdirAll(mediaDir, 0755); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(storeDir, "messages.db")
	mediaPath := filepath.Join(mediaDir, "voice.ogg")
	if err := os.WriteFile(databasePath, []byte("db"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mediaPath, []byte("media"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := secureStorePermissions(storeDir); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	for _, test := range []struct {
		path string
		want os.FileMode
	}{
		{storeDir, 0700},
		{mediaDir, 0700},
		{databasePath, 0600},
		{mediaPath, 0600},
	} {
		info, err := os.Stat(test.path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != test.want {
			t.Errorf("permissions for %s = %04o, want %04o", test.path, got, test.want)
		}
	}
}

func TestBoundedMediaFileStopsOversizedWrites(t *testing.T) {
	underlying, err := os.CreateTemp(t.TempDir(), "bounded-*")
	if err != nil {
		t.Fatal(err)
	}
	defer underlying.Close()
	file := &boundedMediaFile{File: underlying, maxBytes: 4}
	written, err := file.Write([]byte("123456"))
	if written != 4 || !errors.Is(err, errMediaDownloadTooLarge) {
		t.Fatalf("Write() = (%d, %v), want (4, media-too-large)", written, err)
	}
	info, statErr := underlying.Stat()
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Size() != 4 {
		t.Fatalf("bounded file size = %d, want 4", info.Size())
	}
	if err = file.Truncate(5); !errors.Is(err, errMediaDownloadTooLarge) {
		t.Fatalf("Truncate() error = %v, want media-too-large", err)
	}
}

func TestValidateMediaDownloadSize(t *testing.T) {
	if err := validateMediaDownloadSize(uint64(defaultMaxMediaDownloadBytes), defaultMaxMediaDownloadBytes); err != nil {
		t.Fatalf("limit-sized media rejected: %v", err)
	}
	if err := validateMediaDownloadSize(uint64(defaultMaxMediaDownloadBytes)+1, defaultMaxMediaDownloadBytes); !errors.Is(err, errMediaDownloadTooLarge) {
		t.Fatalf("oversized media error = %v, want media-too-large", err)
	}
}

func makeOggPage(sequence uint32, granule uint64, payload []byte) []byte {
	lacing := make([]byte, 0, len(payload)/255+1)
	remaining := len(payload)
	for remaining >= 255 {
		lacing = append(lacing, 255)
		remaining -= 255
	}
	if len(payload) > 0 {
		lacing = append(lacing, byte(remaining))
	}
	page := make([]byte, 27+len(lacing)+len(payload))
	copy(page, "OggS")
	binary.LittleEndian.PutUint64(page[6:14], granule)
	binary.LittleEndian.PutUint32(page[18:22], sequence)
	page[26] = byte(len(lacing))
	copy(page[27:], lacing)
	copy(page[27+len(lacing):], payload)
	return page
}

func TestAnalyzeOggOpusUsesCorrectHeaderOffsets(t *testing.T) {
	const preSkip = 312
	header := make([]byte, 19)
	copy(header, "OpusHead")
	header[8] = 1
	header[9] = 2
	binary.LittleEndian.PutUint16(header[10:12], preSkip)
	binary.LittleEndian.PutUint32(header[12:16], 44100)

	data := append(makeOggPage(0, 0, header), makeOggPage(1, preSkip+48000, nil)...)
	duration, waveform, err := analyzeOggOpus(data)
	if err != nil {
		t.Fatal(err)
	}
	if duration != 1 {
		t.Fatalf("duration = %d, want 1", duration)
	}
	if len(waveform) == 0 {
		t.Fatal("waveform is empty")
	}
}

func TestAnalyzeOggOpusRejectsMalformedBounds(t *testing.T) {
	tests := map[string][]byte{
		"truncated page header": []byte("OggS"),
		"truncated lacing table": func() []byte {
			page := make([]byte, 27)
			copy(page, "OggS")
			page[26] = 1
			return page
		}(),
		"lacing exceeds payload": func() []byte {
			page := make([]byte, 28)
			copy(page, "OggS")
			page[26] = 1
			page[27] = 255
			return page
		}(),
		"truncated OpusHead": makeOggPage(0, 0, []byte("OpusHead\x01")),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := analyzeOggOpus(data); err == nil {
				t.Fatal("analyzeOggOpus() returned nil error")
			}
		})
	}
}

func TestAnalyzeOggOpusAvoidsGranuleUnderflow(t *testing.T) {
	header := make([]byte, 19)
	copy(header, "OpusHead")
	header[8] = 1
	header[9] = 1
	binary.LittleEndian.PutUint16(header[10:12], 1000)

	data := append(makeOggPage(0, 0, header), makeOggPage(1, 500, nil)...)
	duration, _, err := analyzeOggOpus(data)
	if err != nil {
		t.Fatal(err)
	}
	if duration != 1 {
		t.Fatalf("duration = %d, want safe fallback of 1", duration)
	}
}
