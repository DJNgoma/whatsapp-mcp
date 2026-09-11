package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math"
	"math/rand"
	"mime"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"google.golang.org/protobuf/proto"
	"rsc.io/qr"
)

const (
	defaultMaxBridgeRequestBodyBytes = 1 << 20
	defaultMaxMediaDownloadBytes     = 512 << 20
	mediaDownloadOverheadBytes       = 64 << 10
)

var errMediaDownloadTooLarge = errors.New("media download exceeds the configured size limit")

// Message represents a chat message for our client
type Message struct {
	Time      time.Time
	Sender    string
	Content   string
	IsFromMe  bool
	MediaType string
	Filename  string
}

// Database handler for storing message history
type MessageStore struct {
	db       *sql.DB
	storeDir string
}

func ensurePrivateDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	return os.Chmod(directory, 0700)
}

func secureStorePermissions(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		mode := os.FileMode(0600)
		if entry.IsDir() {
			mode = 0700
		} else if !entry.Type().IsRegular() {
			return nil
		}
		return os.Chmod(path, mode)
	})
}

// Initialize message store
func NewMessageStore(storeDir string) (*MessageStore, error) {
	// Create directory for database if it doesn't exist
	if err := ensurePrivateDirectory(storeDir); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %v", err)
	}

	// Open SQLite database for messages
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_foreign_keys=on", filepath.Join(storeDir, "messages.db")))
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}

	// Create tables if they don't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP,
			unread_count INTEGER NOT NULL DEFAULT 0,
			marked_as_unread BOOLEAN NOT NULL DEFAULT FALSE,
			archived BOOLEAN NOT NULL DEFAULT FALSE,
			is_community BOOLEAN NOT NULL DEFAULT FALSE,
			community_jid TEXT,
			is_default_community_chat BOOLEAN NOT NULL DEFAULT FALSE
		);
		
		CREATE TABLE IF NOT EXISTS messages (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			content TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			media_type TEXT,
			filename TEXT,
			url TEXT,
			media_key BLOB,
			file_sha256 BLOB,
			file_enc_sha256 BLOB,
			file_length INTEGER,
			status TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);

		CREATE TABLE IF NOT EXISTS message_quotes (
			id TEXT, chat_jid TEXT, sender_jid TEXT NOT NULL, payload BLOB NOT NULL,
			reply_to_message_id TEXT NOT NULL DEFAULT '', reply_to_sender_jid TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (id,chat_jid)
		);

		CREATE INDEX IF NOT EXISTS idx_messages_chat_timestamp
			ON messages (chat_jid, timestamp DESC);
		CREATE INDEX IF NOT EXISTS idx_messages_timestamp
			ON messages (timestamp DESC);
		CREATE INDEX IF NOT EXISTS idx_chats_last_message_time
			ON chats (last_message_time DESC);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}

	// Older local stores predate chat state metadata. Add the columns without
	// replacing the user's existing message history.
	for _, statement := range []string{
		`ALTER TABLE chats ADD COLUMN unread_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE chats ADD COLUMN marked_as_unread BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE chats ADD COLUMN archived BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE chats ADD COLUMN is_community BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE chats ADD COLUMN community_jid TEXT`,
		`ALTER TABLE chats ADD COLUMN is_default_community_chat BOOLEAN NOT NULL DEFAULT FALSE`,
		`ALTER TABLE messages ADD COLUMN status TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(statement); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("failed to migrate chat metadata: %v", err)
		}
	}
	// Existing outbound rows predate receipt tracking. They were accepted by
	// the bridge, so show them as sent until a newer delivery/read receipt arrives.
	if _, err := db.Exec(`UPDATE messages SET status = 'sent' WHERE is_from_me = TRUE AND COALESCE(status, '') = ''`); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to backfill outbound message status: %v", err)
	}
	if err := secureStorePermissions(storeDir); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to secure store permissions: %v", err)
	}

	return &MessageStore{db: db, storeDir: storeDir}, nil
}

// Close the database connection
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// Store a chat in the database
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)
		 ON CONFLICT(jid) DO UPDATE SET
		 name = CASE WHEN excluded.name != '' THEN excluded.name ELSE chats.name END,
		 last_message_time = excluded.last_message_time`,
		jid, name, lastMessageTime,
	)
	return err
}

func (store *MessageStore) StoreChatState(jid string, unreadCount uint32, markedAsUnread, archived, isCommunity bool, communityJID string, isDefaultCommunityChat bool) error {
	_, err := store.db.Exec(
		`UPDATE chats SET unread_count = ?, marked_as_unread = ?, archived = ?,
		 is_community = ?, community_jid = ?, is_default_community_chat = ? WHERE jid = ?`,
		unreadCount, markedAsUnread, archived, isCommunity, communityJID, isDefaultCommunityChat, jid,
	)
	return err
}

func (store *MessageStore) SetChatReadState(jid string, read bool) error {
	_, err := store.db.Exec(
		`UPDATE chats SET unread_count = ?, marked_as_unread = ? WHERE jid = ?`,
		map[bool]uint32{true: 0, false: 1}[read], !read, jid,
	)
	return err
}

func (store *MessageStore) SetChatArchived(jid string, archived bool) error {
	_, err := store.db.Exec("UPDATE chats SET archived = ? WHERE jid = ?", archived, jid)
	return err
}

// Store a message in the database
func (store *MessageStore) StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	// Only store if there's actual content or media
	if content == "" && mediaType == "" {
		return nil
	}

	_, err := store.db.Exec(
		`INSERT OR REPLACE INTO messages 
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, chatJID, sender, content, timestamp, isFromMe, mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, map[bool]string{true: "sent", false: ""}[isFromMe],
	)
	return err
}

func (store *MessageStore) SetMessageStatus(chatJID, id, status string) error {
	_, err := store.db.Exec("UPDATE messages SET status = ? WHERE chat_jid = ? AND id = ?", status, chatJID, id)
	return err
}

// Get messages from a chat
func (store *MessageStore) GetMessages(chatJID string, limit int) ([]Message, error) {
	rows, err := store.db.Query(
		"SELECT sender, content, timestamp, is_from_me, media_type, filename FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?",
		chatJID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var timestamp time.Time
		err := rows.Scan(&msg.Sender, &msg.Content, &timestamp, &msg.IsFromMe, &msg.MediaType, &msg.Filename)
		if err != nil {
			return nil, err
		}
		msg.Time = timestamp
		messages = append(messages, msg)
	}

	return messages, nil
}

// Get all chats
func (store *MessageStore) GetChats() (map[string]time.Time, error) {
	rows, err := store.db.Query("SELECT jid, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	chats := make(map[string]time.Time)
	for rows.Next() {
		var jid string
		var lastMessageTime time.Time
		err := rows.Scan(&jid, &lastMessageTime)
		if err != nil {
			return nil, err
		}
		chats[jid] = lastMessageTime
	}

	return chats, nil
}

// Extract text content from a message
func extractTextContent(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}

	// Try to get text content
	if text := msg.GetConversation(); text != "" {
		return text
	} else if extendedText := msg.GetExtendedTextMessage(); extendedText != nil {
		return extendedText.GetText()
	} else if image := msg.GetImageMessage(); image != nil {
		return image.GetCaption()
	} else if video := msg.GetVideoMessage(); video != nil {
		return video.GetCaption()
	} else if document := msg.GetDocumentMessage(); document != nil {
		return document.GetCaption()
	}

	// For now, we're ignoring non-text messages
	return ""
}

func formatCallOutcome(value string) string {
	value = strings.ToLower(strings.ReplaceAll(value, "_", " "))
	if value == "" {
		return "completed"
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func formatCallDuration(seconds int64) string {
	if seconds <= 0 {
		return ""
	}
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	return fmt.Sprintf("%dm %02ds", seconds/60, seconds%60)
}

func callMessageSummary(call *waProto.CallLogMessage) string {
	if call == nil {
		return ""
	}
	callKind := "voice"
	if call.GetIsVideo() {
		callKind = "video"
	}
	summary := fmt.Sprintf("%s %s call", formatCallOutcome(call.GetCallOutcome().String()), callKind)
	if duration := formatCallDuration(call.GetDurationSecs()); duration != "" {
		summary += fmt.Sprintf(" · %s", duration)
	}
	return summary
}

func callRecordSummary(record *waSyncAction.CallLogRecord) string {
	if record == nil {
		return ""
	}
	callKind := "voice"
	if record.GetIsVideo() {
		callKind = "video"
	}
	summary := fmt.Sprintf("%s %s call", formatCallOutcome(record.GetCallResult().String()), callKind)
	if duration := formatCallDuration(record.GetDuration()); duration != "" {
		summary += fmt.Sprintf(" · %s", duration)
	}
	return summary
}

func storeCallEvent(client *whatsmeow.Client, messageStore *MessageStore, eventBroker *eventBroker, logger waLog.Logger, callID, chatJID string, timestamp time.Time, summary string) {
	if chatJID == "" || timestamp.IsZero() {
		return
	}
	if callID == "" {
		callID = fmt.Sprintf("%s-%d", chatJID, timestamp.Unix())
	}
	sender := strings.TrimSuffix(strings.Split(chatJID, "@")[0], ":0")
	isFromMe := false
	if client.Store.ID != nil && sender == client.Store.ID.User {
		isFromMe = true
	}
	chat := mustParseJID(chatJID)
	name := GetChatName(client, messageStore, chat, chatJID, nil, sender, logger)
	if err := messageStore.StoreChat(chatJID, name, timestamp); err != nil {
		return
	}
	messageID := "call-" + callID
	if err := messageStore.StoreMessage(messageID, chatJID, sender, summary, timestamp, isFromMe, "call", "", "", nil, nil, nil, 0); err == nil {
		eventBroker.publish(bridgeEvent{
			Type: "message", ID: messageID, ChatJID: chatJID, ChatName: name, Sender: sender,
			PhoneNumber: eventPhoneNumber(client, chat),
			Content:     summary, Timestamp: timestamp.Format(time.RFC3339), IsFromMe: isFromMe, MediaType: "call",
		})
	}
}

func mustParseJID(value string) types.JID {
	jid, _ := types.ParseJID(value)
	return jid
}

func eventPhoneNumber(client *whatsmeow.Client, jid types.JID) string {
	if jid.Server == types.DefaultUserServer {
		return jid.User
	}
	if jid.Server == types.HiddenUserServer {
		if phoneJID, err := client.Store.LIDs.GetPNForLID(context.Background(), jid); err == nil {
			return phoneJID.User
		}
	}
	return ""
}

// SendMessageResponse represents the response for the send message API
type SendMessageResponse struct {
	Success   bool   `json:"success"`
	Message   string `json:"message"`
	MessageID string `json:"message_id,omitempty"`
}

// SendMessageRequest represents the request body for the send message API
type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	MediaPath string `json:"media_path,omitempty"`
}

func portableBaseName(filePath string) string {
	// filepath.Base follows the host OS. Media paths can originate in a separate
	// MCP process, so accept either Windows or POSIX separators on every host.
	return path.Base(strings.ReplaceAll(filePath, `\`, "/"))
}

func mediaTypeAndMIME(filePath string) (whatsmeow.MediaType, string) {
	extension := strings.ToLower(path.Ext(strings.ReplaceAll(filePath, `\`, "/")))
	switch extension {
	case ".jpg", ".jpeg":
		return whatsmeow.MediaImage, "image/jpeg"
	case ".png":
		return whatsmeow.MediaImage, "image/png"
	case ".gif":
		return whatsmeow.MediaImage, "image/gif"
	case ".webp":
		return whatsmeow.MediaImage, "image/webp"
	case ".ogg":
		return whatsmeow.MediaAudio, "audio/ogg; codecs=opus"
	case ".mp4":
		return whatsmeow.MediaVideo, "video/mp4"
	case ".avi":
		return whatsmeow.MediaVideo, "video/avi"
	case ".mov":
		return whatsmeow.MediaVideo, "video/quicktime"
	default:
		mimeType := mime.TypeByExtension(extension)
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		return whatsmeow.MediaDocument, mimeType
	}
}

func newDocumentMessage(mediaPath, caption, mimeType string, upload whatsmeow.UploadResponse) *waProto.DocumentMessage {
	fileName := portableBaseName(mediaPath)
	return &waProto.DocumentMessage{
		FileName:      proto.String(fileName),
		Title:         proto.String(fileName),
		Caption:       proto.String(caption),
		Mimetype:      proto.String(mimeType),
		URL:           &upload.URL,
		DirectPath:    &upload.DirectPath,
		MediaKey:      upload.MediaKey,
		FileEncSHA256: upload.FileEncSHA256,
		FileSHA256:    upload.FileSHA256,
		FileLength:    &upload.FileLength,
	}
}

type HealthResponse struct {
	Status                  string `json:"status"`
	Connected               bool   `json:"connected"`
	LoggedIn                bool   `json:"logged_in"`
	LatestStoredMessageTime string `json:"latest_stored_message_time,omitempty"`
	ServerTime              string `json:"server_time"`
	InstanceID              string `json:"instance_id,omitempty"`
}

func configuredBridgeInstanceID() string {
	return os.Getenv("WHATSAPP_BRIDGE_INSTANCE_ID")
}

func positiveInt64Environment(name string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func configuredMaxBridgeRequestBodyBytes() int64 {
	return positiveInt64Environment("WHATSAPP_BRIDGE_MAX_REQUEST_BODY_BYTES", defaultMaxBridgeRequestBodyBytes)
}

func configuredMaxMediaDownloadBytes() int64 {
	configured := positiveInt64Environment("WHATSAPP_BRIDGE_MAX_MEDIA_DOWNLOAD_BYTES", defaultMaxMediaDownloadBytes)
	if configured > math.MaxInt64-mediaDownloadOverheadBytes {
		return defaultMaxMediaDownloadBytes
	}
	return configured
}

func isLoopbackHost(hostPort string) bool {
	hostPort = strings.TrimSpace(hostPort)
	if hostPort == "" {
		return false
	}
	host := hostPort
	if parsedHost, _, err := net.SplitHostPort(hostPort); err == nil {
		host = parsedHost
	}
	host = strings.Trim(strings.TrimSuffix(strings.ToLower(host), "."), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func protectLoopbackAPI(next http.Handler, maxBodyBytes int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if strings.TrimSpace(r.Header.Get("Origin")) != "" {
			http.Error(w, "Browser-originated requests are not allowed", http.StatusForbidden)
			return
		}
		if maxBodyBytes > 0 {
			if r.ContentLength > maxBodyBytes {
				http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// Function to send a WhatsApp message
func sendWhatsAppMessage(client *whatsmeow.Client, recipient string, message string, mediaPath string) (bool, string, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp", ""
	}

	// Create JID for recipient
	var recipientJID types.JID
	var err error

	// Check if recipient is a JID
	isJID := strings.Contains(recipient, "@")

	if isJID {
		// Parse the JID string
		recipientJID, err = types.ParseJID(recipient)
		if err != nil {
			return false, fmt.Sprintf("Error parsing JID: %v", err), ""
		}
	} else {
		// Create JID from phone number
		recipientJID = types.JID{
			User:   recipient,
			Server: "s.whatsapp.net", // For personal chats
		}
	}

	msg := &waProto.Message{}

	// Check if we have media to send
	if mediaPath != "" {
		// Read media file
		mediaData, err := os.ReadFile(mediaPath)
		if err != nil {
			return false, fmt.Sprintf("Error reading media file: %v", err), ""
		}

		mediaType, mimeType := mediaTypeAndMIME(mediaPath)

		// Upload media to WhatsApp servers
		resp, err := client.Upload(context.Background(), mediaData, mediaType)
		if err != nil {
			return false, fmt.Sprintf("Error uploading media: %v", err), ""
		}

		// Create the appropriate message type based on media type
		switch mediaType {
		case whatsmeow.MediaImage:
			msg.ImageMessage = &waProto.ImageMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaAudio:
			// Handle ogg audio files
			var seconds uint32 = 30 // Default fallback
			var waveform []byte = nil

			// Try to analyze the ogg file
			if strings.Contains(mimeType, "ogg") {
				analyzedSeconds, analyzedWaveform, err := analyzeOggOpus(mediaData)
				if err == nil {
					seconds = analyzedSeconds
					waveform = analyzedWaveform
				} else {
					return false, fmt.Sprintf("Failed to analyze Ogg Opus file: %v", err), ""
				}
			} else {
				fmt.Printf("Not an Ogg Opus file: %s\n", mimeType)
			}

			msg.AudioMessage = &waProto.AudioMessage{
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
				Seconds:       proto.Uint32(seconds),
				PTT:           proto.Bool(true),
				Waveform:      waveform,
			}
		case whatsmeow.MediaVideo:
			msg.VideoMessage = &waProto.VideoMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaDocument:
			msg.DocumentMessage = newDocumentMessage(mediaPath, message, mimeType, resp)
		}
	} else {
		msg.Conversation = proto.String(message)
	}

	// Send message
	response, err := client.SendMessage(context.Background(), recipientJID, msg)

	if err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err), ""
	}

	return true, fmt.Sprintf("Message sent to %s", recipient), response.ID
}

// Extract media info from a message
func extractMediaInfo(msg *waProto.Message) (mediaType string, filename string, url string, mediaKey []byte, fileSHA256 []byte, fileEncSHA256 []byte, fileLength uint64) {
	if msg == nil {
		return "", "", "", nil, nil, nil, 0
	}

	// Check for image message
	if img := msg.GetImageMessage(); img != nil {
		return "image", "image_" + time.Now().Format("20060102_150405") + ".jpg",
			img.GetURL(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}

	// Check for video message
	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", "video_" + time.Now().Format("20060102_150405") + ".mp4",
			vid.GetURL(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}

	// Check for audio message
	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", "audio_" + time.Now().Format("20060102_150405") + ".ogg",
			aud.GetURL(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}

	// Check for document message
	if doc := msg.GetDocumentMessage(); doc != nil {
		filename := doc.GetFileName()
		if filename == "" {
			filename = "document_" + time.Now().Format("20060102_150405")
		}
		return "document", filename,
			doc.GetURL(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}

	return "", "", "", nil, nil, nil, 0
}

// Handle regular incoming messages with media support
func handleMessage(client *whatsmeow.Client, messageStore *MessageStore, eventBroker *eventBroker, msg *events.Message, logger waLog.Logger, logMessages bool) {
	// Save message to database
	chatJID := msg.Info.Chat.String()
	sender := msg.Info.Sender.User

	// Get appropriate chat name (pass nil for conversation since we don't have one for regular messages)
	name := GetChatName(client, messageStore, msg.Info.Chat, chatJID, nil, sender, logger)

	// Update chat in database with the message timestamp (keeps last message time updated)
	err := messageStore.StoreChat(chatJID, name, msg.Info.Timestamp)
	if err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}

	// Extract text content
	content := extractTextContent(msg.Message)

	// Extract media info
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg.Message)
	if call := msg.Message.GetCallLogMesssage(); call != nil {
		content = callMessageSummary(call)
		mediaType = "call"
		filename = ""
		url = ""
		mediaKey = nil
		fileSHA256 = nil
		fileEncSHA256 = nil
		fileLength = 0
	}

	// Skip if there's no content and no media
	if content == "" && mediaType == "" {
		return
	}

	// Store message in database
	err = messageStore.StoreMessage(
		msg.Info.ID,
		chatJID,
		sender,
		content,
		msg.Info.Timestamp,
		msg.Info.IsFromMe,
		mediaType,
		filename,
		url,
		mediaKey,
		fileSHA256,
		fileEncSHA256,
		fileLength,
	)

	if err != nil {
		logger.Warnf("Failed to store message: %v", err)
	} else {
		if quoteErr := messageStore.storeQuote(msg.Info.ID, chatJID, msg.Info.Sender.ToNonAD().String(), msg.Message); quoteErr != nil {
			logger.Debugf("Quote payload unavailable: %v", quoteErr)
		}
		eventBroker.publish(bridgeEvent{
			Type: "message", ID: msg.Info.ID, ChatJID: chatJID, ChatName: name, Sender: sender,
			PhoneNumber: eventPhoneNumber(client, msg.Info.Chat),
			Content:     content, Timestamp: msg.Info.Timestamp.Format(time.RFC3339), IsFromMe: msg.Info.IsFromMe,
			MediaType: mediaType, Filename: filename,
		})
	}
	if err == nil && logMessages {
		// Log message reception
		timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
		direction := "←"
		if msg.Info.IsFromMe {
			direction = "→"
		}

		// Log based on message type
		if mediaType != "" {
			fmt.Printf("[%s] %s %s: [%s: %s] %s\n", timestamp, direction, sender, mediaType, filename, content)
		} else if content != "" {
			fmt.Printf("[%s] %s %s: %s\n", timestamp, direction, sender, content)
		}
	}
}

// DownloadMediaRequest represents the request body for the download media API
type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

// DownloadMediaResponse represents the response for the download media API
type DownloadMediaResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
}

// Store additional media info in the database
func (store *MessageStore) StoreMediaInfo(id, chatJID, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	_, err := store.db.Exec(
		"UPDATE messages SET url = ?, media_key = ?, file_sha256 = ?, file_enc_sha256 = ?, file_length = ? WHERE id = ? AND chat_jid = ?",
		url, mediaKey, fileSHA256, fileEncSHA256, fileLength, id, chatJID,
	)
	return err
}

// Get media info from the database
func (store *MessageStore) GetMediaInfo(id, chatJID string) (string, string, string, []byte, []byte, []byte, uint64, error) {
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64

	err := store.db.QueryRow(
		"SELECT media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&mediaType, &filename, &url, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength)

	return mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err
}

// MediaDownloader implements the whatsmeow.DownloadableMessage interface
type MediaDownloader struct {
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileLength    uint64
	FileSHA256    []byte
	FileEncSHA256 []byte
	MediaType     whatsmeow.MediaType
}

type boundedMediaFile struct {
	*os.File
	maxBytes int64
}

func (file *boundedMediaFile) Write(data []byte) (int, error) {
	offset, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	return file.writeAtMost(data, offset, file.File.Write)
}

func (file *boundedMediaFile) WriteAt(data []byte, offset int64) (int, error) {
	return file.writeAtMost(data, offset, func(chunk []byte) (int, error) {
		return file.File.WriteAt(chunk, offset)
	})
}

func (file *boundedMediaFile) Truncate(size int64) error {
	if size < 0 || size > file.maxBytes {
		return errMediaDownloadTooLarge
	}
	return file.File.Truncate(size)
}

func (file *boundedMediaFile) writeAtMost(data []byte, offset int64, write func([]byte) (int, error)) (int, error) {
	if offset < 0 || offset >= file.maxBytes {
		return 0, errMediaDownloadTooLarge
	}
	remaining := file.maxBytes - offset
	if int64(len(data)) <= remaining {
		return write(data)
	}
	written, err := write(data[:remaining])
	if err != nil {
		return written, err
	}
	return written, errMediaDownloadTooLarge
}

func validateMediaDownloadSize(fileLength uint64, maxBytes int64) error {
	if fileLength == 0 {
		return fmt.Errorf("media file length is missing")
	}
	if maxBytes <= 0 || fileLength > uint64(maxBytes) {
		return fmt.Errorf("%w: declared size is %d bytes (limit %d)", errMediaDownloadTooLarge, fileLength, maxBytes)
	}
	return nil
}

// GetDirectPath implements the DownloadableMessage interface
func (d *MediaDownloader) GetDirectPath() string {
	return d.DirectPath
}

// GetURL implements the DownloadableMessage interface
func (d *MediaDownloader) GetURL() string {
	return d.URL
}

// GetMediaKey implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaKey() []byte {
	return d.MediaKey
}

// GetFileLength implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileLength() uint64 {
	return d.FileLength
}

// GetFileSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileSHA256() []byte {
	return d.FileSHA256
}

// GetFileEncSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileEncSHA256() []byte {
	return d.FileEncSHA256
}

// GetMediaType implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaType() whatsmeow.MediaType {
	return d.MediaType
}

// Function to download media from a message
func downloadMedia(ctx context.Context, client *whatsmeow.Client, messageStore *MessageStore, messageID, chatJID string) (bool, string, string, string, error) {
	// Query the database for the message
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64
	var err error

	// First, check if we already have this file
	chatDir := filepath.Join(messageStore.storeDir, strings.ReplaceAll(chatJID, ":", "_"))
	localPath := ""

	// Get media info from the database
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err = messageStore.GetMediaInfo(messageID, chatJID)

	if err != nil {
		// Try to get basic info if extended info isn't available
		err = messageStore.db.QueryRow(
			"SELECT media_type, filename FROM messages WHERE id = ? AND chat_jid = ?",
			messageID, chatJID,
		).Scan(&mediaType, &filename)

		if err != nil {
			return false, "", "", "", fmt.Errorf("failed to find message: %v", err)
		}
	}

	// Check if this is a media message
	if mediaType == "" {
		return false, "", "", "", fmt.Errorf("not a media message")
	}

	// Create a private directory for downloaded media, tightening permissions on
	// stores created by earlier versions.
	if err := ensurePrivateDirectory(chatDir); err != nil {
		return false, "", "", "", fmt.Errorf("failed to create chat directory: %v", err)
	}

	// Generate a local path for the file
	// WhatsApp can assign the same generated filename to several media rows.
	// Keep each message in a distinct local file so one download cannot overwrite another.
	storageFilename := uniqueMediaFilename(messageID, filename)
	localPath = filepath.Join(chatDir, storageFilename)

	// Get absolute path
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to get absolute path: %v", err)
	}

	// Check if file already exists
	if info, statErr := os.Lstat(localPath); statErr == nil {
		if !info.Mode().IsRegular() {
			return false, "", "", "", fmt.Errorf("existing media path is not a regular file")
		}
		if err := os.Chmod(localPath, 0600); err != nil {
			return false, "", "", "", fmt.Errorf("failed to secure existing media file: %v", err)
		}
		return true, mediaType, filename, absPath, nil
	} else if !os.IsNotExist(statErr) {
		return false, "", "", "", fmt.Errorf("failed to inspect media path: %v", statErr)
	}

	// If we don't have all the media info we need, we can't download
	if url == "" || len(mediaKey) == 0 || len(fileSHA256) == 0 || len(fileEncSHA256) == 0 || fileLength == 0 {
		return false, "", "", "", fmt.Errorf("incomplete media information for download")
	}
	maxDownloadBytes := configuredMaxMediaDownloadBytes()
	if err := validateMediaDownloadSize(fileLength, maxDownloadBytes); err != nil {
		return false, "", "", "", err
	}

	fmt.Println("Downloading WhatsApp media...")

	// Extract direct path from URL
	directPath := extractDirectPathFromURL(url)

	// Create a downloader that implements DownloadableMessage
	var waMediaType whatsmeow.MediaType
	switch mediaType {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "document":
		waMediaType = whatsmeow.MediaDocument
	default:
		return false, "", "", "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	downloader := &MediaDownloader{
		URL:           url,
		DirectPath:    directPath,
		MediaKey:      mediaKey,
		FileLength:    fileLength,
		FileSHA256:    fileSHA256,
		FileEncSHA256: fileEncSHA256,
		MediaType:     waMediaType,
	}

	temporaryFile, err := os.CreateTemp(chatDir, ".whatsapp-download-*")
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to create temporary media file: %v", err)
	}
	temporaryPath := temporaryFile.Name()
	keepTemporary := false
	defer func() {
		_ = temporaryFile.Close()
		if !keepTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()

	limitedFile := &boundedMediaFile{File: temporaryFile, maxBytes: maxDownloadBytes + mediaDownloadOverheadBytes}
	if err = client.DownloadToFile(ctx, downloader, limitedFile); err != nil {
		return false, "", "", "", fmt.Errorf("failed to download media: %v", err)
	}
	fileInfo, err := temporaryFile.Stat()
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to inspect downloaded media: %v", err)
	}
	if fileInfo.Size() > maxDownloadBytes {
		return false, "", "", "", fmt.Errorf("%w: downloaded size is %d bytes (limit %d)", errMediaDownloadTooLarge, fileInfo.Size(), maxDownloadBytes)
	}
	if err = temporaryFile.Chmod(0600); err != nil {
		return false, "", "", "", fmt.Errorf("failed to secure downloaded media: %v", err)
	}
	if err = temporaryFile.Close(); err != nil {
		return false, "", "", "", fmt.Errorf("failed to close downloaded media: %v", err)
	}
	if err = os.Rename(temporaryPath, localPath); err != nil {
		return false, "", "", "", fmt.Errorf("failed to finalize downloaded media: %v", err)
	}
	keepTemporary = true

	fmt.Printf("Downloaded %s media (%d bytes)\n", mediaType, fileInfo.Size())
	return true, mediaType, filename, absPath, nil
}

func uniqueMediaFilename(messageID, filename string) string {
	if filename == "" {
		return filename
	}
	filename = portableBaseName(filename)
	if messageID == "" {
		return filename
	}
	extension := path.Ext(filename)
	base := strings.TrimSuffix(filename, extension)
	return fmt.Sprintf("%s_%s%s", base, safeMediaFilenameComponent(messageID), extension)
}

func safeMediaFilenameComponent(value string) string {
	const maxReadableBytes = 96

	var sanitized strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '-' || character == '_' {
			sanitized.WriteRune(character)
		} else {
			sanitized.WriteByte('_')
		}
	}

	component := sanitized.String()
	changed := component != value
	if component == "" {
		component = "message"
		changed = true
	}
	if len(component) > maxReadableBytes {
		component = component[:maxReadableBytes]
		changed = true
	}
	if !changed {
		return component
	}

	// Keep sanitized or truncated identifiers collision-resistant so distinct
	// WhatsApp messages cannot overwrite one another after normalization.
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%s-%x", component, digest[:8])
}

// Extract direct path from a WhatsApp media URL
func extractDirectPathFromURL(url string) string {
	// WhatsApp media URLs include signed query parameters. DownloadMediaWithPath
	// appends its own parameters to this value, so preserve the original query.
	parsed, err := neturl.Parse(url)
	if err != nil || parsed.Path == "" {
		return url
	}

	return parsed.RequestURI()
}

// Start a REST API server to expose the WhatsApp client functionality
func startRESTServer(client *whatsmeow.Client, messageStore *MessageStore, eventBroker *eventBroker, port int) (*http.Server, error) {
	mux := http.NewServeMux()
	registerCapabilityHandlers(mux, client, messageStore)
	registerEventStreamHandler(mux, eventBroker)
	registerReplyHandlers(mux, client, messageStore, eventBroker)

	// Health is read-only and reports both live connectivity and local cache age.
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var latestMessage sql.NullString
		_ = messageStore.db.QueryRow("SELECT MAX(timestamp) FROM messages").Scan(&latestMessage)

		status := "degraded"
		if client.IsConnected() && client.IsLoggedIn() {
			status = "ok"
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(HealthResponse{
			Status:                  status,
			Connected:               client.IsConnected(),
			LoggedIn:                client.IsLoggedIn(),
			LatestStoredMessageTime: latestMessage.String,
			ServerTime:              time.Now().Format(time.RFC3339),
			InstanceID:              configuredBridgeInstanceID(),
		})
	})

	// Handler for sending messages
	mux.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body
		var req SendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.Recipient == "" {
			http.Error(w, "Recipient is required", http.StatusBadRequest)
			return
		}

		if req.Message == "" && req.MediaPath == "" {
			http.Error(w, "Message or media path is required", http.StatusBadRequest)
			return
		}

		fmt.Println("Received outbound WhatsApp request")

		// Send the message
		success, message, messageID := sendWhatsAppMessage(client, req.Recipient, req.Message, req.MediaPath)
		if success && messageID != "" {
			chatJID := req.Recipient
			if parsed, parseErr := types.ParseJID(req.Recipient); parseErr == nil {
				chatJID = parsed.String()
			}
			sender := ""
			if client.Store.ID != nil {
				sender = client.Store.ID.User
			}
			timestamp := time.Now()
			_ = messageStore.StoreChat(chatJID, chatJID, timestamp)
			if err := messageStore.StoreMessage(messageID, chatJID, sender, req.Message, timestamp, true, "", "", "", nil, nil, nil, 0); err == nil {
				eventBroker.publish(bridgeEvent{
					Type: "message", ID: messageID, ChatJID: chatJID, Sender: sender,
					Content: req.Message, Timestamp: timestamp.Format(time.RFC3339), IsFromMe: true,
				})
			}
		}
		fmt.Println("Outbound WhatsApp request completed:", success)
		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Set appropriate status code
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		// Send response
		json.NewEncoder(w).Encode(SendMessageResponse{
			Success:   success,
			Message:   message,
			MessageID: messageID,
		})
	})

	// Handler for downloading media
	mux.HandleFunc("/api/download", func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body
		var req DownloadMediaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.MessageID == "" || req.ChatJID == "" {
			http.Error(w, "Message ID and Chat JID are required", http.StatusBadRequest)
			return
		}

		// Download the media
		success, mediaType, filename, path, err := downloadMedia(r.Context(), client, messageStore, req.MessageID, req.ChatJID)

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Handle download result
		if !success || err != nil {
			errMsg := "Unknown error"
			if err != nil {
				errMsg = err.Error()
			}

			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(DownloadMediaResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to download media: %s", errMsg),
			})
			return
		}

		// Send successful response
		json.NewEncoder(w).Encode(DownloadMediaResponse{
			Success:  true,
			Message:  fmt.Sprintf("Successfully downloaded %s media", mediaType),
			Filename: filename,
			Path:     path,
		})
	})

	// Start the server
	serverAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	fmt.Printf("Starting REST API server on %s...\n", serverAddr)
	listener, err := net.Listen("tcp", serverAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", serverAddr, err)
	}

	server := &http.Server{
		Addr:              serverAddr,
		Handler:           protectLoopbackAPI(mux, configuredMaxBridgeRequestBodyBytes()),
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	// Run server in a goroutine so it doesn't block
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Printf("REST API server error: %v\n", err)
		}
	}()

	return server, nil
}

func defaultBridgePort() int {
	const fallbackPort = 8741
	portValue := os.Getenv("WHATSAPP_BRIDGE_PORT")
	if portValue == "" {
		return fallbackPort
	}

	port, err := strconv.Atoi(portValue)
	if err != nil || port < 1 || port > 65535 {
		fmt.Fprintf(os.Stderr, "Ignoring invalid WHATSAPP_BRIDGE_PORT %q; using %d\n", portValue, fallbackPort)
		return fallbackPort
	}

	return port
}

func defaultStoreDir() string {
	if storeDir := os.Getenv("WHATSAPP_STORE_DIR"); storeDir != "" {
		return filepath.Clean(storeDir)
	}
	return "store"
}

func fillRoundedRectangle(dst *image.RGBA, rect image.Rectangle, radius int, fill color.Color) {
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			insideHorizontal := x >= rect.Min.X+radius && x < rect.Max.X-radius
			insideVertical := y >= rect.Min.Y+radius && y < rect.Max.Y-radius
			if insideHorizontal || insideVertical {
				dst.Set(x, y, fill)
				continue
			}

			centerX := rect.Min.X + radius
			if x >= rect.Max.X-radius {
				centerX = rect.Max.X - radius - 1
			}
			centerY := rect.Min.Y + radius
			if y >= rect.Max.Y-radius {
				centerY = rect.Max.Y - radius - 1
			}
			dx, dy := x-centerX, y-centerY
			if dx*dx+dy*dy <= radius*radius {
				dst.Set(x, y, fill)
			}
		}
	}
}

func rasterizeQRCode(code *qr.Code) *image.RGBA {
	const quietZoneModules = 4
	dimension := (code.Size + 2*quietZoneModules) * code.Scale
	canvas := image.NewRGBA(image.Rect(0, 0, dimension, dimension))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	black := image.NewUniform(color.Black)

	for moduleY := 0; moduleY < code.Size; moduleY++ {
		for moduleX := 0; moduleX < code.Size; moduleX++ {
			if !code.Black(moduleX, moduleY) {
				continue
			}
			pixelX := (moduleX + quietZoneModules) * code.Scale
			pixelY := (moduleY + quietZoneModules) * code.Scale
			moduleRect := image.Rect(pixelX, pixelY, pixelX+code.Scale, pixelY+code.Scale)
			draw.Draw(canvas, moduleRect, black, image.Point{}, draw.Src)
		}
	}

	return canvas
}

func renderBrandedQRCode(code *qr.Code, label string) ([]byte, error) {
	parsedFont, err := opentype.Parse(gobold.TTF)
	if err != nil {
		return nil, fmt.Errorf("failed to load QR label font: %w", err)
	}

	labelFace, err := opentype.NewFace(parsedFont, &opentype.FaceOptions{
		Size:    24,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create QR label font: %w", err)
	}
	defer labelFace.Close()

	canvas := rasterizeQRCode(code)
	qrBounds := canvas.Bounds()

	metrics := labelFace.Metrics()
	labelWidth := font.MeasureString(labelFace, label).Ceil()
	labelHeight := (metrics.Ascent + metrics.Descent).Ceil()
	const horizontalPadding = 24
	const verticalPadding = 14
	const borderWidth = 3
	const quietSpace = 10
	badgeWidth := labelWidth + 2*horizontalPadding
	badgeHeight := labelHeight + 2*verticalPadding
	badge := image.Rect(
		(qrBounds.Dx()-badgeWidth)/2,
		(qrBounds.Dy()-badgeHeight)/2,
		(qrBounds.Dx()+badgeWidth)/2,
		(qrBounds.Dy()+badgeHeight)/2,
	)
	whatsAppGreen := color.RGBA{R: 37, G: 211, B: 102, A: 255}
	quietBadge := badge.Inset(-quietSpace)
	fillRoundedRectangle(canvas, quietBadge, quietBadge.Dy()/2, color.White)
	fillRoundedRectangle(canvas, badge, badgeHeight/2, whatsAppGreen)
	innerBadge := badge.Inset(borderWidth)
	fillRoundedRectangle(canvas, innerBadge, innerBadge.Dy()/2, color.White)

	labelX := badge.Min.X + (badgeWidth-labelWidth)/2
	labelBaseline := badge.Min.Y + (badgeHeight-labelHeight)/2 + metrics.Ascent.Ceil()
	labelDrawer := font.Drawer{
		Dst:  canvas,
		Src:  image.NewUniform(whatsAppGreen),
		Face: labelFace,
		Dot:  fixed.P(labelX, labelBaseline),
	}
	labelDrawer.DrawString(label)

	var output bytes.Buffer
	if err = png.Encode(&output, canvas); err != nil {
		return nil, fmt.Errorf("failed to encode labeled QR image: %w", err)
	}
	return output.Bytes(), nil
}

func openQRCodeImage(contents string) (string, error) {
	code, err := qr.Encode(contents, qr.M)
	if err != nil {
		return "", fmt.Errorf("failed to encode QR code: %w", err)
	}
	code.Scale = 8
	labeledPNG, err := renderBrandedQRCode(code, "whatsmeow")
	if err != nil {
		return "", err
	}

	imagePath := filepath.Join(
		os.TempDir(),
		fmt.Sprintf("whatsmeow-pairing-qr-%d.png", time.Now().UnixNano()),
	)
	if err = os.WriteFile(imagePath, labeledPNG, 0600); err != nil {
		return "", fmt.Errorf("failed to write QR code image: %w", err)
	}

	command, args, err := imageOpenCommand(runtime.GOOS, imagePath)
	if err != nil {
		return imagePath, err
	}

	if err = exec.Command(command, args...).Start(); err != nil {
		return imagePath, fmt.Errorf("failed to open QR code image: %w", err)
	}

	return imagePath, nil
}

func imageOpenCommand(goos, imagePath string) (string, []string, error) {
	switch goos {
	case "darwin":
		return "open", []string{imagePath}, nil
	case "linux":
		return "xdg-open", []string{imagePath}, nil
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", imagePath}, nil
	default:
		return "", nil, fmt.Errorf("automatic image opening is not supported on %s", goos)
	}
}

type pairingIdentity struct {
	osName        string
	clientType    whatsmeow.PairClientType
	clientDisplay string
}

func identityForOS(goos string) pairingIdentity {
	switch goos {
	case "darwin":
		return pairingIdentity{"macOS", whatsmeow.PairClientMacOS, "WhatsApp MCP (macOS)"}
	case "windows":
		return pairingIdentity{"Windows", whatsmeow.PairClientUWP, "WhatsApp MCP (Windows)"}
	default:
		return pairingIdentity{"Linux", whatsmeow.PairClientChrome, "WhatsApp MCP (Linux)"}
	}
}

func main() {
	ctx := context.Background()
	phoneNumber := flag.String("phone", "", "pair using a phone number in international format, for example +27821234567")
	port := flag.Int("port", defaultBridgePort(), "localhost REST API port")
	storeDir := flag.String("store-dir", defaultStoreDir(), "directory for login state, messages, and downloaded media")
	logLevel := flag.String("log-level", "INFO", "whatsmeow log level (DEBUG, INFO, WARN, ERROR)")
	logMessages := flag.Bool("log-messages", false, "include private message content in console logs")
	flag.Parse()

	// Set up logger
	logger := waLog.Stdout("Client", *logLevel, true)
	logger.Infof("Starting WhatsApp client...")

	// Create database connection for storing session data
	dbLog := waLog.Stdout("Database", *logLevel, true)

	// Create directory for database if it doesn't exist
	if err := ensurePrivateDirectory(*storeDir); err != nil {
		logger.Errorf("Failed to create store directory: %v", err)
		return
	}

	container, err := sqlstore.New(ctx, "sqlite3", fmt.Sprintf("file:%s?_foreign_keys=on", filepath.Join(*storeDir, "whatsapp.db")), dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
	}
	if err = secureStorePermissions(*storeDir); err != nil {
		logger.Errorf("Failed to secure store permissions: %v", err)
		return
	}

	// Get device store - This contains session information
	deviceStore, err := container.GetFirstDevice(ctx)
	if err != nil {
		if err == sql.ErrNoRows {
			// No device exists, create one
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return
		}
	}

	// Identify new linked-device registrations using the host platform. Existing
	// linked sessions retain their stored identity.
	pairing := identityForOS(runtime.GOOS)
	store.DeviceProps.Os = proto.String(pairing.osName)
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()

	// Create client instance
	client := whatsmeow.NewClient(deviceStore, logger)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}
	client.QRClientType = pairing.clientType

	// Initialize message store
	messageStore, err := NewMessageStore(*storeDir)
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return
	}
	defer messageStore.Close()
	eventBroker := newEventBroker(256)

	// Setup event handling for messages and history sync
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			// Process regular messages
			handleMessage(client, messageStore, eventBroker, v, logger, *logMessages)

		case *events.HistorySync:
			// Process history sync events
			handleHistorySync(client, messageStore, v, logger, *logMessages)

		case *events.CallOffer:
			chatJID := v.From.String()
			if !v.GroupJID.IsEmpty() {
				chatJID = v.GroupJID.String()
			}
			storeCallEvent(client, messageStore, eventBroker, logger, v.CallID, chatJID, v.Timestamp, "Missed voice call")

		case *events.CallAccept:
			chatJID := v.From.String()
			if !v.GroupJID.IsEmpty() {
				chatJID = v.GroupJID.String()
			}
			storeCallEvent(client, messageStore, eventBroker, logger, v.CallID, chatJID, v.Timestamp, "Completed voice call")

		case *events.CallTerminate:
			chatJID := v.From.String()
			if !v.GroupJID.IsEmpty() {
				chatJID = v.GroupJID.String()
			}
			storeCallEvent(client, messageStore, eventBroker, logger, v.CallID, chatJID, v.Timestamp, "Ended voice call")

		case *events.MarkChatAsRead:
			if err := messageStore.SetChatReadState(v.JID.String(), v.Action.GetRead()); err != nil {
				logger.Warnf("Failed to store chat read state: %v", err)
			}

		case *events.Archive:
			if err := messageStore.SetChatArchived(v.JID.String(), v.Action.GetArchived()); err != nil {
				logger.Warnf("Failed to store chat archive state: %v", err)
			}

		case *events.Receipt:
			status := "delivered"
			switch v.Type {
			case types.ReceiptTypeRead, types.ReceiptTypeReadSelf:
				status = "read"
			case types.ReceiptTypeRetry:
				status = "failed"
			case types.ReceiptTypeSender:
				status = "sent"
			}
			for _, messageID := range v.MessageIDs {
				if err := messageStore.SetMessageStatus(v.Chat.String(), messageID, status); err != nil {
					logger.Warnf("Failed to store receipt status for %s: %v", messageID, err)
				} else {
					eventBroker.publish(bridgeEvent{Type: "receipt", ChatJID: v.Chat.String(), Status: status, MessageIDs: []string{messageID}})
				}
			}

		case *events.Connected:
			logger.Infof("Connected to WhatsApp")

		case *events.LoggedOut:
			logger.Warnf("Device logged out, please pair the device again")
		}
	})

	// Connect to WhatsApp
	if client.Store.ID == nil {
		// No ID stored, this is a new client and must be paired.
		pairingCtx, cancelPairing := context.WithTimeout(ctx, 3*time.Minute)
		defer cancelPairing()

		qrChan, err := client.GetQRChannel(pairingCtx)
		if err != nil {
			logger.Errorf("Failed to initialize pairing: %v", err)
			return
		}

		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}

		authenticated := false
		pairCodeRequested := false
		qrImagePath := ""
		cleanupQRCodeImage := func() {
			if qrImagePath != "" {
				_ = os.Remove(qrImagePath)
				qrImagePath = ""
			}
		}

		for evt := range qrChan {
			switch evt.Event {
			case whatsmeow.QRChannelEventCode:
				if *phoneNumber == "" {
					cleanupQRCodeImage()
					imagePath, imageErr := openQRCodeImage(evt.Code)
					if imageErr != nil {
						if imagePath != "" {
							_ = os.Remove(imagePath)
						}
						logger.Warnf("Could not open QR code as an image: %v", imageErr)
						fmt.Println("\nScan this QR code with your WhatsApp app:")
						qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
					} else {
						qrImagePath = imagePath
						fmt.Printf("\nOpened a fresh WhatsApp pairing QR code in your system image viewer: %s\n", imagePath)
						fmt.Printf("This code expires in approximately %s. Always scan the newest QR image.\n", evt.Timeout.Round(time.Second))
						fmt.Println("1. Scan the QR image with your phone's Camera app.")
						fmt.Println("2. Tap the WhatsApp link, then tap Continue.")
						fmt.Println("3. Scan the newest QR image again inside WhatsApp to confirm linking.")
					}
				} else if !pairCodeRequested {
					pairCodeRequested = true
					code, pairErr := client.PairPhone(
						pairingCtx,
						*phoneNumber,
						true,
						pairing.clientType,
						pairing.clientDisplay,
					)
					if pairErr != nil {
						logger.Errorf("Failed to generate phone pairing code: %v", pairErr)
						return
					}

					fmt.Printf("\nYour WhatsApp pairing code is: %s\n", code)
					fmt.Println("On your phone, open WhatsApp > Settings > Linked Devices > Link a Device > Link with phone number instead, then enter this code.")
				}
			case "success":
				authenticated = true
			case whatsmeow.QRChannelEventError:
				cleanupQRCodeImage()
				logger.Errorf("Pairing failed: %v", evt.Error)
				return
			default:
				cleanupQRCodeImage()
				logger.Errorf("Pairing ended with status: %s", evt.Event)
				return
			}

			if authenticated {
				break
			}
		}

		if !authenticated {
			cleanupQRCodeImage()
			logger.Errorf("Pairing ended before authentication completed")
			return
		}

		cancelPairing()
		cleanupQRCodeImage()
		fmt.Println("\nSuccessfully connected and authenticated!")
	} else {
		// Already logged in, just connect
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}
	}

	// Wait a moment for connection to stabilize
	time.Sleep(2 * time.Second)

	if !client.IsConnected() {
		logger.Errorf("Failed to establish stable connection")
		return
	}

	fmt.Println("\n✓ Connected to WhatsApp.")

	// Start REST API server
	server, err := startRESTServer(client, messageStore, eventBroker, *port)
	if err != nil {
		logger.Errorf("Failed to start REST API server: %v", err)
		client.Disconnect()
		return
	}

	// Create a channel to keep the main goroutine alive
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running.")

	// Wait for termination signal
	<-exitChan

	fmt.Println("Disconnecting...")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warnf("REST API shutdown failed: %v", err)
	}
	// Disconnect client
	client.Disconnect()
}

// GetChatName determines the appropriate name for a chat based on JID and other info
func GetChatName(client *whatsmeow.Client, messageStore *MessageStore, jid types.JID, chatJID string, conversation interface{}, sender string, logger waLog.Logger) string {
	// First, check if chat already exists in database with a name
	var existingName string
	err := messageStore.db.QueryRow("SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existingName)
	if err == nil && existingName != "" && !(jid.Server == types.DefaultUserServer && isNumericChatName(existingName)) {
		// Chat exists with a name, use that
		logger.Infof("Using existing chat name for %s: %s", chatJID, existingName)
		return existingName
	}

	// Need to determine chat name
	var name string

	if jid.Server == "g.us" {
		// This is a group chat
		logger.Infof("Getting name for group: %s", chatJID)

		// Use conversation data if provided (from history sync)
		if conversation != nil {
			// Extract name from conversation if available
			// This uses type assertions to handle different possible types
			var displayName, convName *string
			// Try to extract the fields we care about regardless of the exact type
			v := reflect.ValueOf(conversation)
			if v.Kind() == reflect.Ptr && !v.IsNil() {
				v = v.Elem()

				// Try to find DisplayName field
				if displayNameField := v.FieldByName("DisplayName"); displayNameField.IsValid() && displayNameField.Kind() == reflect.Ptr && !displayNameField.IsNil() {
					dn := displayNameField.Elem().String()
					displayName = &dn
				}

				// Try to find Name field
				if nameField := v.FieldByName("Name"); nameField.IsValid() && nameField.Kind() == reflect.Ptr && !nameField.IsNil() {
					n := nameField.Elem().String()
					convName = &n
				}
			}

			// Use the name we found
			if displayName != nil && *displayName != "" {
				name = *displayName
			} else if convName != nil && *convName != "" {
				name = *convName
			}
		}

		// If we didn't get a name, try group info
		if name == "" {
			groupInfo, err := client.GetGroupInfo(context.Background(), jid)
			if err == nil && groupInfo.Name != "" {
				name = groupInfo.Name
			} else {
				// Fallback name for groups
				name = fmt.Sprintf("Group %s", jid.User)
			}
		}

		logger.Infof("Using group name: %s", name)
	} else {
		// This is an individual contact
		logger.Infof("Getting name for contact: %s", chatJID)

		// Just use contact info (full name)
		contact, err := client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil && contact.FullName != "" {
			name = contact.FullName
		} else if sender != "" {
			// Fallback to sender
			name = sender
		} else {
			// Last fallback to JID
			name = jid.User
		}

		logger.Infof("Using contact name: %s", name)
	}

	return name
}

func isNumericChatName(value string) bool {
	digits := 0
	for _, character := range value {
		if character >= '0' && character <= '9' {
			digits++
			continue
		}
		if character == '+' || character == '-' || character == '(' || character == ')' || character == ' ' || character == '.' {
			continue
		}
		return false
	}
	return digits >= 7
}

// Handle history sync events
func handleHistorySync(client *whatsmeow.Client, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger, logMessages bool) {
	fmt.Printf("Received history sync event with %d conversations\n", len(historySync.Data.Conversations))

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		// Parse JID from the conversation
		if conversation.ID == nil {
			continue
		}

		chatJID := *conversation.ID

		// Try to parse the JID
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", chatJID, err)
			continue
		}

		// Get appropriate chat name by passing the history sync conversation directly
		name := GetChatName(client, messageStore, jid, chatJID, conversation, "", logger)

		// Process messages
		messages := conversation.Messages
		if len(messages) > 0 {
			// Update chat with latest message timestamp
			latestMsg := messages[0]
			if latestMsg == nil || latestMsg.Message == nil {
				continue
			}

			// Get timestamp from message info
			latestTimestamp := latestMsg.Message.GetMessageTimestamp()
			if latestTimestamp == 0 {
				continue
			}
			timestamp := time.Unix(int64(latestTimestamp), 0)

			messageStore.StoreChat(chatJID, name, timestamp)
			if err := messageStore.StoreChatState(
				chatJID,
				conversation.GetUnreadCount(),
				conversation.GetMarkedAsUnread(),
				conversation.GetArchived(),
				conversation.GetIsParentGroup(),
				conversation.GetParentGroupID(),
				conversation.GetIsDefaultSubgroup(),
			); err != nil {
				logger.Warnf("Failed to store chat state for %s: %v", chatJID, err)
			}

			// Store messages
			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
				}

				// Extract text and media captions.
				content := extractTextContent(msg.Message.Message)

				// Extract media info
				var mediaType, filename, url string
				var mediaKey, fileSHA256, fileEncSHA256 []byte
				var fileLength uint64

				if msg.Message.Message != nil {
					mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength = extractMediaInfo(msg.Message.Message)
					if call := msg.Message.Message.GetCallLogMesssage(); call != nil {
						content = callMessageSummary(call)
						mediaType = "call"
						filename = ""
						url = ""
						mediaKey = nil
						fileSHA256 = nil
						fileEncSHA256 = nil
						fileLength = 0
					}
				}

				if logMessages {
					logger.Infof("Message content: %v, Media Type: %v", content, mediaType)
				}

				// Skip messages with no content and no media
				if content == "" && mediaType == "" {
					continue
				}

				// Determine sender
				var sender string
				isFromMe := false
				if msg.Message.Key != nil {
					if msg.Message.Key.FromMe != nil {
						isFromMe = *msg.Message.Key.FromMe
					}
					if !isFromMe && msg.Message.Key.Participant != nil && *msg.Message.Key.Participant != "" {
						sender = *msg.Message.Key.Participant
					} else if isFromMe {
						sender = client.Store.ID.User
					} else {
						sender = jid.User
					}
				} else {
					sender = jid.User
				}

				// Store message
				msgID := ""
				if msg.Message.Key != nil && msg.Message.Key.ID != nil {
					msgID = *msg.Message.Key.ID
				}

				// Get message timestamp
				messageTimestamp := msg.Message.GetMessageTimestamp()
				if messageTimestamp == 0 {
					continue
				}
				timestamp := time.Unix(int64(messageTimestamp), 0)

				err = messageStore.StoreMessage(
					msgID,
					chatJID,
					sender,
					content,
					timestamp,
					isFromMe,
					mediaType,
					filename,
					url,
					mediaKey,
					fileSHA256,
					fileEncSHA256,
					fileLength,
				)
				if err != nil {
					logger.Warnf("Failed to store history message: %v", err)
				} else {
					quoteSender := sender
					if isFromMe {
						quoteSender = client.Store.ID.ToNonAD().String()
					} else if !strings.Contains(sender, "@") && jid.Server != types.GroupServer {
						quoteSender = jid.ToNonAD().String()
					}
					if strings.Contains(quoteSender, "@") {
						_ = messageStore.storeQuote(msgID, chatJID, quoteSender, msg.Message.Message)
					}
					syncedCount++
					if logMessages {
						if mediaType != "" {
							logger.Infof("Stored message: [%s] %s -> %s: [%s: %s] %s",
								timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, mediaType, filename, content)
						} else {
							logger.Infof("Stored message: [%s] %s -> %s: %s",
								timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, content)
						}
					}
				}
			}
		}
	}

	// Call logs are delivered separately from conversation messages during history sync.
	for _, record := range historySync.Data.CallLogRecords {
		if record == nil || record.GetStartTime() == 0 {
			continue
		}

		chatJID := record.GetGroupJID()
		if chatJID == "" && len(record.GetParticipants()) > 0 {
			chatJID = record.GetParticipants()[0].GetUserJID()
		}
		if chatJID == "" {
			continue
		}

		jid, err := types.ParseJID(chatJID)
		if err != nil {
			logger.Warnf("Failed to parse call chat JID %s: %v", chatJID, err)
			continue
		}
		name := GetChatName(client, messageStore, jid, chatJID, nil, jid.User, logger)
		timestamp := time.Unix(record.GetStartTime(), 0)
		messageID := record.GetCallID()
		if messageID == "" {
			messageID = fmt.Sprintf("%s-%d-%s", chatJID, record.GetStartTime(), record.GetCallResult().String())
		}
		sender := jid.User
		if !record.GetIsIncoming() && client.Store.ID != nil {
			sender = client.Store.ID.User
		}

		if err := messageStore.StoreChat(chatJID, name, timestamp); err != nil {
			logger.Warnf("Failed to store call chat: %v", err)
			continue
		}
		if err := messageStore.StoreMessage(
			"call-"+messageID,
			chatJID,
			sender,
			callRecordSummary(record),
			timestamp,
			!record.GetIsIncoming(),
			"call",
			"",
			"",
			nil,
			nil,
			nil,
			0,
		); err != nil {
			logger.Warnf("Failed to store history call: %v", err)
		} else {
			syncedCount++
		}
	}

	fmt.Printf("History sync complete. Stored %d messages.\n", syncedCount)
}

// Request history sync from the server
func requestHistorySync(client *whatsmeow.Client, messageStore *MessageStore, chatJID string, fromLatest bool) error {
	if client == nil || !client.IsConnected() || client.Store.ID == nil {
		return fmt.Errorf("WhatsApp client is not connected")
	}
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return fmt.Errorf("invalid chat JID: %w", err)
	}
	var messageID string
	var timestamp time.Time
	var isFromMe bool
	order := "ASC"
	if fromLatest {
		order = "DESC"
	}
	if err := messageStore.db.QueryRow(
		"SELECT id, timestamp, is_from_me FROM messages WHERE chat_jid = ? ORDER BY timestamp "+order+", id "+order+" LIMIT 1", chatJID,
	).Scan(&messageID, &timestamp, &isFromMe); err != nil {
		return fmt.Errorf("no stored oldest message for %s: %w", chatJID, err)
	}
	request := client.BuildHistorySyncRequest(&types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, IsFromMe: isFromMe},
		ID:            messageID,
		Timestamp:     timestamp,
	}, 50)
	if request == nil {
		return fmt.Errorf("failed to build history sync request")
	}
	if _, err := client.SendPeerMessage(context.Background(), request); err != nil {
		return fmt.Errorf("failed to request history sync: %w", err)
	}
	fmt.Printf("History sync requested for %s before %s.\n", chatJID, messageID)
	return nil
}

// analyzeOggOpus tries to extract duration and generate a simple waveform from an Ogg Opus file
func analyzeOggOpus(data []byte) (duration uint32, waveform []byte, err error) {
	if len(data) < 4 || string(data[0:4]) != "OggS" {
		return 0, nil, fmt.Errorf("not a valid Ogg file (missing OggS signature)")
	}

	var lastGranule uint64
	var preSkip uint16
	var foundOpusHead bool
	pageCount := 0

	for i := 0; i < len(data); {
		if len(data)-i < 27 {
			return 0, nil, fmt.Errorf("truncated Ogg page header at offset %d", i)
		}
		if string(data[i:i+4]) != "OggS" {
			return 0, nil, fmt.Errorf("invalid Ogg capture pattern at offset %d", i)
		}
		if data[i+4] != 0 {
			return 0, nil, fmt.Errorf("unsupported Ogg stream version %d", data[i+4])
		}

		granulePos := binary.LittleEndian.Uint64(data[i+6 : i+14])
		pageSeqNum := binary.LittleEndian.Uint32(data[i+18 : i+22])
		numSegments := int(data[i+26])
		headerEnd := i + 27 + numSegments
		if headerEnd > len(data) {
			return 0, nil, fmt.Errorf("truncated Ogg lacing table at offset %d", i)
		}
		segmentTable := data[i+27 : i+27+numSegments]
		payloadLength := 0
		for _, segLen := range segmentTable {
			payloadLength += int(segLen)
		}
		pageEnd := headerEnd + payloadLength
		if pageEnd > len(data) {
			return 0, nil, fmt.Errorf("ogg lacing table exceeds available payload at offset %d", i)
		}

		if !foundOpusHead && pageSeqNum <= 1 {
			pageData := data[headerEnd:pageEnd]
			headPos := bytes.Index(pageData, []byte("OpusHead"))
			if headPos >= 0 {
				if len(pageData)-headPos < 19 {
					return 0, nil, fmt.Errorf("truncated OpusHead packet")
				}
				channels := pageData[headPos+9]
				if channels == 0 {
					return 0, nil, fmt.Errorf("invalid OpusHead channel count")
				}
				preSkip = binary.LittleEndian.Uint16(pageData[headPos+10 : headPos+12])
				inputSampleRate := binary.LittleEndian.Uint32(pageData[headPos+12 : headPos+16])
				foundOpusHead = true
				fmt.Printf("Found OpusHead: inputSampleRate=%d, preSkip=%d\n", inputSampleRate, preSkip)
			}
		}

		if granulePos != 0 && granulePos != math.MaxUint64 {
			lastGranule = granulePos
		}
		pageCount++
		i = pageEnd
	}

	if pageCount == 0 {
		return 0, nil, fmt.Errorf("ogg file contains no complete pages")
	}
	if !foundOpusHead {
		return 0, nil, fmt.Errorf("opus head packet not found")
	}

	if lastGranule > uint64(preSkip) {
		// Opus granule positions are always measured at 48 kHz. The input sample
		// rate in OpusHead is informational and must not be used for duration.
		durationSeconds := float64(lastGranule-uint64(preSkip)) / 48000.0
		if durationSeconds >= 300 {
			duration = 300
		} else {
			duration = uint32(math.Ceil(durationSeconds))
		}
		fmt.Printf("Calculated Opus duration from granule: %f seconds (lastGranule=%d)\n",
			durationSeconds, lastGranule)
	} else {
		fmt.Println("Warning: No valid granule position found, using estimation")
		durationEstimate := float64(len(data)) / 2000.0 // Very rough approximation
		duration = uint32(durationEstimate)
	}

	if duration < 1 {
		duration = 1
	} else if duration > 300 {
		duration = 300
	}

	// Generate waveform
	waveform = placeholderWaveform(duration)

	fmt.Printf("Ogg Opus analysis: size=%d bytes, calculated duration=%d sec, waveform=%d bytes\n",
		len(data), duration, len(waveform))

	return duration, waveform, nil
}

// min returns the smaller of x or y
func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

// placeholderWaveform generates a synthetic waveform for WhatsApp voice messages
// that appears natural with some variability based on the duration
func placeholderWaveform(duration uint32) []byte {
	// WhatsApp expects a 64-byte waveform for voice messages
	const waveformLength = 64
	waveform := make([]byte, waveformLength)

	// Use a local generator for deterministic output without mutating global random state.
	random := rand.New(rand.NewSource(int64(duration)))

	// Create a more natural looking waveform with some patterns and variability
	// rather than completely random values

	// Base amplitude and frequency - longer messages get faster frequency
	baseAmplitude := 35.0
	frequencyFactor := float64(min(int(duration), 120)) / 30.0

	for i := range waveform {
		// Position in the waveform (normalized 0-1)
		pos := float64(i) / float64(waveformLength)

		// Create a wave pattern with some randomness
		// Use multiple sine waves of different frequencies for more natural look
		val := baseAmplitude * math.Sin(pos*math.Pi*frequencyFactor*8)
		val += (baseAmplitude / 2) * math.Sin(pos*math.Pi*frequencyFactor*16)

		// Add some randomness to make it look more natural
		val += (random.Float64() - 0.5) * 15

		// Add some fade-in and fade-out effects
		fadeInOut := math.Sin(pos * math.Pi)
		val = val * (0.7 + 0.3*fadeInOut)

		// Center around 50 (typical voice baseline)
		val = val + 50

		// Ensure values stay within WhatsApp's expected range (0-100)
		if val < 0 {
			val = 0
		} else if val > 100 {
			val = 100
		}

		waveform[i] = byte(val)
	}

	return waveform
}
