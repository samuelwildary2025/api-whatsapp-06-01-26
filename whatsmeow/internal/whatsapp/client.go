package whatsapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	_ "github.com/mattn/go-sqlite3"
)

// Instance represents a WhatsApp connection instance
type Instance struct {
	ID           string
	Client       *whatsmeow.Client
	Device       *store.Device
	Status       string
	QRCode       string
	QRCodeBase64 string
	PairingCode  string
	WANumber     string
	WAName       string

	// Settings
	RejectCalls  bool // Auto-reject incoming calls
	AlwaysOnline bool // Keep presence as online 24h
	IgnoreGroups bool // Don't process group messages
	SyncHistory  bool // Request full history sync on connect
	ReadMessages bool // Auto mark messages as read

	// Proxy configuration
	ProxyHost     string
	ProxyPort     string
	ProxyUsername string
	ProxyPassword string
	ProxyProtocol string // http, https, socks4, socks5

	mu sync.RWMutex
}

// RLock locks instance for reading
func (i *Instance) RLock() {
	i.mu.RLock()
}

// RUnlock unlocks instance read lock
func (i *Instance) RUnlock() {
	i.mu.RUnlock()
}

// Manager manages multiple WhatsApp instances
type Manager struct {
	instances   map[string]*Instance
	container   *sqlstore.Container
	dataDir     string
	mu          sync.RWMutex
	eventSubs   map[string][]chan Event
	eventSubsMu sync.RWMutex

	mapping     map[string]string // InstanceID -> JIDString
	mappingFile string

	// Message storage for each chat
	messages   map[string]map[string][]MessageData // instanceID -> chatID -> messages
	messagesMu sync.RWMutex
}

// Event represents a WhatsApp event
type Event struct {
	Type       string      `json:"type"`
	InstanceID string      `json:"instanceId"`
	Data       interface{} `json:"data"`
	Timestamp  int64       `json:"timestamp"`
}

// MessageData represents message data
type MessageData struct {
	ID            string `json:"id"`
	From          string `json:"from"`
	To            string `json:"to"`
	Body          string `json:"body"`
	Type          string `json:"type"`
	Timestamp     int64  `json:"timestamp"`
	FromMe        bool   `json:"fromMe"`
	IsGroup       bool   `json:"isGroup"`
	PushName      string `json:"pushName,omitempty"`
	ResolvedPhone string `json:"resolvedPhone,omitempty"`
	// Media fields
	MediaBase64 string `json:"mediaBase64,omitempty"`
	Mimetype    string `json:"mimetype,omitempty"`
	Caption     string `json:"caption,omitempty"`
	FileName    string `json:"fileName,omitempty"`
}

// ResolvedContactInfo represents resolved contact information
type ResolvedContactInfo struct {
	OriginalJID   string `json:"originalJid"`
	ResolvedPhone string `json:"resolvedPhone,omitempty"`
	PushName      string `json:"pushName,omitempty"`
	FullName      string `json:"fullName,omitempty"`
	IsLID         bool   `json:"isLid"`
	Resolved      bool   `json:"resolved"`
}

// NewManager creates a new WhatsApp manager
func NewManager(dataDir string) (*Manager, error) {
	// Create SQLite store for sessions
	dbPath := fmt.Sprintf("%s/whatsmeow.db", dataDir)
	dbLog := waLog.Stdout("Database", "WARN", true)

	container, err := sqlstore.New(context.Background(), "sqlite3", fmt.Sprintf("file:%s?_foreign_keys=on", dbPath), dbLog)
	if err != nil {
		return nil, fmt.Errorf("failed to create database: %w", err)
	}

	m := &Manager{
		instances:   make(map[string]*Instance),
		container:   container,
		dataDir:     dataDir,
		eventSubs:   make(map[string][]chan Event),
		mapping:     make(map[string]string),
		mappingFile: fmt.Sprintf("%s/instances.json", dataDir),
		messages:    make(map[string]map[string][]MessageData),
	}

	// Load mapping
	m.loadMapping()

	// Restore sessions
	m.restoreSessions()

	return m, nil
}

// loadMapping loads instance mapping from file
func (m *Manager) loadMapping() {
	data, err := os.ReadFile(m.mappingFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Error().Err(err).Msg("Failed to load instance mapping")
		}
		return
	}

	if err := json.Unmarshal(data, &m.mapping); err != nil {
		log.Error().Err(err).Msg("Failed to unmarshal instance mapping")
	}
}

// saveMapping saves instance mapping to file
func (m *Manager) saveMapping() {
	data, err := json.MarshalIndent(m.mapping, "", "  ")
	if err != nil {
		log.Error().Err(err).Msg("Failed to marshal instance mapping")
		return
	}

	if err := os.WriteFile(m.mappingFile, data, 0644); err != nil {
		log.Error().Err(err).Msg("Failed to save instance mapping")
	}
}

// restoreSessions restores sessions from mapping
func (m *Manager) restoreSessions() {
	log.Info().Msg("Restoring sessions...")

	for instanceID, jidStr := range m.mapping {
		jid, err := types.ParseJID(jidStr)
		if err != nil {
			log.Error().Err(err).Str("instanceId", instanceID).Str("jid", jidStr).Msg("Invalid JID in mapping")
			continue
		}

		device, err := m.container.GetDevice(context.Background(), jid)
		if err != nil {
			log.Error().Err(err).Str("instanceId", instanceID).Msg("Failed to get device from store")
			continue
		}

		if device == nil {
			log.Warn().Str("instanceId", instanceID).Msg("Device not found in store, skipping")
			continue
		}

		// Recreate instance
		clientLog := waLog.Stdout("Client-"+instanceID, "INFO", true)
		client := whatsmeow.NewClient(device, clientLog)

		instance := &Instance{
			ID:     instanceID,
			Client: client,
			Device: device,
			Status: "disconnected", // Will update on connect
		}

		instance.WANumber = jid.User
		instance.WAName = device.PushName

		m.setupEventHandlers(instance)

		if err := client.Connect(); err != nil {
			log.Error().Err(err).Str("instanceId", instanceID).Msg("Failed to connect restored session")
		} else {
			instance.Status = "connected"
			log.Info().Str("instanceId", instanceID).Msg("Session restored and connected")
		}

		m.instances[instanceID] = instance
	}
}

// GetOrCreateInstance gets existing instance or creates new one
func (m *Manager) GetOrCreateInstance(instanceID string) (*Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if already exists in memory
	if inst, ok := m.instances[instanceID]; ok {
		return inst, nil
	}

	// Check if mapped (but not loaded for some reason, e.g. disconnect)
	if jidStr, ok := m.mapping[instanceID]; ok {
		// Logic similar to restore...
		// Actually restoreSessions runs at startup. If it's not in m.instances, it's not active.
		// Try to load from store again just in case
		jid, _ := types.ParseJID(jidStr)
		if device, err := m.container.GetDevice(context.Background(), jid); err == nil && device != nil {
			clientLog := waLog.Stdout("Client-"+instanceID, "INFO", true)
			client := whatsmeow.NewClient(device, clientLog)
			instance := &Instance{
				ID:     instanceID,
				Client: client,
				Device: device,
				Status: "disconnected",
			}
			m.setupEventHandlers(instance)
			m.instances[instanceID] = instance
			return instance, nil
		}
	}

	// Create new device
	device := m.container.NewDevice()

	// Always enforce Safari/Mac OS identity
	device.Platform = "Mac OS X" // Changed from "Mac OS" to see if it fixes "Outros"
	device.BusinessName = "Safari"

	// Create client
	clientLog := waLog.Stdout("Client-"+instanceID, "INFO", true)
	client := whatsmeow.NewClient(device, clientLog)

	instance := &Instance{
		ID:     instanceID,
		Client: client,
		Device: device,
		Status: "disconnected",
	}

	// Setup event handlers
	m.setupEventHandlers(instance)

	m.instances[instanceID] = instance
	return instance, nil
}

// setupEventHandlers sets up WhatsApp event handlers for an instance
func (m *Manager) setupEventHandlers(inst *Instance) {
	inst.Client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.QR:
			// Generate QR code
			qrCode := v.Codes[0]
			inst.mu.Lock()
			inst.Status = "qr"
			inst.QRCode = qrCode

			// Generate base64 QR image
			png, err := qrcode.Encode(qrCode, qrcode.Medium, 256)
			if err == nil {
				inst.QRCodeBase64 = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
			}
			inst.mu.Unlock()

			log.Info().Str("instanceId", inst.ID).Msg("QR code generated")
			m.publishEvent(Event{
				Type:       "qr",
				InstanceID: inst.ID,
				Data: map[string]string{
					"qr":       qrCode,
					"qrBase64": inst.QRCodeBase64,
				},
			})

		case *events.PairSuccess:
			inst.mu.Lock()
			inst.WANumber = v.ID.User
			inst.mu.Unlock()

			// Save mapping
			m.mu.Lock()
			m.mapping[inst.ID] = v.ID.String()
			m.saveMapping()
			m.mu.Unlock()

			log.Info().Str("instanceId", inst.ID).Str("number", inst.WANumber).Msg("WhatsApp paired successfully")

		case *events.Connected:
			inst.mu.Lock()
			inst.Status = "connected"
			inst.QRCode = ""
			inst.QRCodeBase64 = ""
			if inst.Client.Store.ID != nil {
				inst.WANumber = inst.Client.Store.ID.User
			}
			inst.WAName = inst.Client.Store.PushName
			inst.mu.Unlock()

			log.Info().Str("instanceId", inst.ID).Str("number", inst.WANumber).Msg("WhatsApp connected")
			m.publishEvent(Event{
				Type:       "ready",
				InstanceID: inst.ID,
				Data: map[string]string{
					"number": inst.WANumber,
					"name":   inst.WAName,
				},
			})

		case *events.Disconnected:
			inst.mu.Lock()
			inst.Status = "disconnected"
			inst.mu.Unlock()

			log.Warn().Str("instanceId", inst.ID).Msg("WhatsApp disconnected")
			m.publishEvent(Event{
				Type:       "disconnected",
				InstanceID: inst.ID,
				Data:       nil,
			})

		case *events.LoggedOut:
			inst.mu.Lock()
			inst.Status = "disconnected"
			inst.WANumber = ""
			inst.WAName = ""
			inst.mu.Unlock()

			log.Warn().Str("instanceId", inst.ID).Msg("WhatsApp logged out")
			m.publishEvent(Event{
				Type:       "logged_out",
				InstanceID: inst.ID,
				Data:       nil,
			})

		case *events.Message:
			// Check if we should ignore group messages
			inst.mu.RLock()
			ignoreGroups := inst.IgnoreGroups
			readMessages := inst.ReadMessages
			inst.mu.RUnlock()

			if ignoreGroups && v.Info.IsGroup {
				log.Debug().Str("instanceId", inst.ID).Msg("Ignoring group message (setting enabled)")
				return
			}

			msgData := m.formatMessage(inst.ID, v)
			log.Debug().Str("instanceId", inst.ID).Str("from", msgData.From).Msg("Message received")
			// Store the message
			m.storeMessage(inst.ID, msgData.To, msgData)

			// Auto mark as read if enabled
			if readMessages && !v.Info.IsFromMe {
				go func() {
					err := inst.Client.MarkRead(context.Background(), []types.MessageID{v.Info.ID}, time.Now(), v.Info.Chat, v.Info.Sender)
					if err != nil {
						log.Warn().Err(err).Msg("Failed to mark message as read")
					}
				}()
			}

			m.publishEvent(Event{
				Type:       "message",
				InstanceID: inst.ID,
				Data:       msgData,
			})

		case *events.HistorySync:
			// Process history sync to capture historical messages
			// NOTE: We use formatMessageLite to avoid downloading media for historical messages
			log.Info().Str("instanceId", inst.ID).Int("conversations", len(v.Data.GetConversations())).Msg("Received history sync")

			for _, conv := range v.Data.GetConversations() {
				chatJID := conv.GetID()
				for _, historyMsg := range conv.GetMessages() {
					webMsg := historyMsg.GetMessage()
					if webMsg == nil {
						continue
					}

					// Parse the web message to get message data
					parsedMsg, err := inst.Client.ParseWebMessage(types.JID{}, webMsg)
					if err != nil {
						log.Warn().Err(err).Msg("Failed to parse history message")
						continue
					}

					// Use formatMessageLite to avoid downloading media for historical messages
					msgData := m.formatMessageLite(inst.ID, parsedMsg)
					m.storeMessage(inst.ID, chatJID, msgData)
				}
			}

			m.publishEvent(Event{
				Type:       "history_sync",
				InstanceID: inst.ID,
				Data: map[string]interface{}{
					"conversations": len(v.Data.GetConversations()),
				},
			})

		case *events.Receipt:
			m.publishEvent(Event{
				Type:       "message_ack",
				InstanceID: inst.ID,
				Data: map[string]interface{}{
					"messageIds": v.MessageIDs,
					"type":       fmt.Sprintf("%v", v.Type),
					"from":       v.MessageSource.Sender.String(),
				},
			})

		case *events.CallOffer:
			log.Info().Str("instanceId", inst.ID).Str("from", v.CallCreator.String()).Str("callId", v.CallID).Msg("Incoming call")

			// Publish call event
			m.publishEvent(Event{
				Type:       "call",
				InstanceID: inst.ID,
				Data: map[string]interface{}{
					"from":   v.CallCreator.String(),
					"callId": v.CallID,
					"type":   "offer",
				},
			})

			// Auto-reject if enabled
			inst.mu.RLock()
			shouldReject := inst.RejectCalls
			inst.mu.RUnlock()

			if shouldReject {
				// Run rejection in goroutine with slight delay to ensure call is established
				go func(callCreator types.JID, callID string) {
					// Small delay to ensure call is properly established
					time.Sleep(500 * time.Millisecond)

					log.Info().Str("instanceId", inst.ID).Str("callId", callID).Str("from", callCreator.String()).Msg("Auto-rejecting call")

					err := inst.Client.RejectCall(context.Background(), callCreator, callID)
					if err != nil {
						log.Error().Err(err).Str("callId", callID).Msg("Failed to reject call")
					} else {
						log.Info().Str("callId", callID).Msg("Call rejected successfully")
					}
				}(v.CallCreator, v.CallID)
			}
		}
	})
}

// formatMessage formats a WhatsApp message event
func (m *Manager) formatMessage(instanceID string, msg *events.Message) MessageData {
	var body string
	var msgType string = "text"
	var mediaBase64 string
	var mimetype string
	var caption string
	var fileName string

	// Get instance for media download
	inst, _ := m.GetInstance(instanceID)

	// Check for different message types
	if msg.Message.GetConversation() != "" {
		body = msg.Message.GetConversation()
	} else if msg.Message.GetExtendedTextMessage() != nil {
		body = msg.Message.GetExtendedTextMessage().GetText()
	} else if imgMsg := msg.Message.GetImageMessage(); imgMsg != nil {
		msgType = "image"
		caption = imgMsg.GetCaption()
		mimetype = imgMsg.GetMimetype()
		body = caption
		// Download image
		if inst != nil && inst.Client != nil {
			data, err := inst.Client.Download(context.Background(), imgMsg)
			if err != nil {
				log.Warn().Err(err).Msg("Failed to download image")
			} else {
				mediaBase64 = base64.StdEncoding.EncodeToString(data)
				log.Info().Str("instanceId", instanceID).Int("bytes", len(data)).Msg("Image downloaded successfully")
			}
		}
	} else if vidMsg := msg.Message.GetVideoMessage(); vidMsg != nil {
		msgType = "video"
		caption = vidMsg.GetCaption()
		mimetype = vidMsg.GetMimetype()
		body = caption
		// Download video
		if inst != nil && inst.Client != nil {
			data, err := inst.Client.Download(context.Background(), vidMsg)
			if err != nil {
				log.Warn().Err(err).Msg("Failed to download video")
			} else {
				mediaBase64 = base64.StdEncoding.EncodeToString(data)
				log.Info().Str("instanceId", instanceID).Int("bytes", len(data)).Msg("Video downloaded successfully")
			}
		}
	} else if audioMsg := msg.Message.GetAudioMessage(); audioMsg != nil {
		msgType = "audio"
		mimetype = audioMsg.GetMimetype()
		// Download audio
		if inst != nil && inst.Client != nil {
			data, err := inst.Client.Download(context.Background(), audioMsg)
			if err != nil {
				log.Warn().Err(err).Msg("Failed to download audio")
			} else {
				mediaBase64 = base64.StdEncoding.EncodeToString(data)
				log.Info().Str("instanceId", instanceID).Int("bytes", len(data)).Msg("Audio downloaded successfully")
			}
		}
	} else if docMsg := msg.Message.GetDocumentMessage(); docMsg != nil {
		msgType = "document"
		caption = docMsg.GetCaption()
		mimetype = docMsg.GetMimetype()
		fileName = docMsg.GetFileName()
		body = caption
		// Download document
		if inst != nil && inst.Client != nil {
			data, err := inst.Client.Download(context.Background(), docMsg)
			if err != nil {
				log.Warn().Err(err).Msg("Failed to download document")
			} else {
				mediaBase64 = base64.StdEncoding.EncodeToString(data)
				log.Info().Str("instanceId", instanceID).Int("bytes", len(data)).Msg("Document downloaded successfully")
			}
		}
	} else if stickerMsg := msg.Message.GetStickerMessage(); stickerMsg != nil {
		msgType = "sticker"
		mimetype = stickerMsg.GetMimetype()
		// Download sticker
		if inst != nil && inst.Client != nil {
			data, err := inst.Client.Download(context.Background(), stickerMsg)
			if err != nil {
				log.Warn().Err(err).Msg("Failed to download sticker")
			} else {
				mediaBase64 = base64.StdEncoding.EncodeToString(data)
				log.Info().Str("instanceId", instanceID).Int("bytes", len(data)).Msg("Sticker downloaded successfully")
			}
		}
	}

	senderJID := msg.Info.Sender.String()
	resolvedPhone := ""

	// Attempt to resolve LID to phone number
	if strings.HasSuffix(senderJID, "@lid") {
		log.Info().Str("lid", senderJID).Msg("Processing message from LID contact - starting resolution")

		if inst != nil && inst.Client != nil && inst.Client.Store != nil {
			// 1. Try LIDs table
			if inst.Client.Store.LIDs != nil {
				lidJID := msg.Info.Sender
				pnJID, err := inst.Client.Store.LIDs.GetPNForLID(context.Background(), lidJID)
				if err == nil && pnJID.User != "" {
					resolvedPhone = pnJID.User
					log.Info().Str("lid", senderJID).Str("resolvedPhone", resolvedPhone).Msg("✅ Resolved LID via Store.LIDs")
				} else {
					log.Info().Str("lid", senderJID).Err(err).Msg("❌ Failed to resolve via Store.LIDs")
				}
			}

			// 2. If failed, try Contacts table (sometimes they are linked there)
			if resolvedPhone == "" && inst.Client.Store.Contacts != nil {
				contact, err := inst.Client.Store.Contacts.GetContact(context.Background(), msg.Info.Sender)
				if err == nil {
					log.Info().
						Str("lid", senderJID).
						Str("foundName", contact.FullName).
						Str("foundPushName", contact.PushName).
						Msg("ℹ️ Found contact in Store.Contacts")
				}
			}

			// 3. Fallback: GetUserInfo (at least get the name)
			if resolvedPhone == "" {
				users, err := inst.Client.GetUserInfo(context.Background(), []types.JID{msg.Info.Sender})
				if err == nil {
					if user, ok := users[msg.Info.Sender]; ok {
						vName := ""
						if user.VerifiedName != nil {
							vName = "present"
						}
						log.Info().
							Str("lid", senderJID).
							Str("verifiedName", vName).
							Str("status", user.Status).
							Str("pictureID", user.PictureID). // Available in UserInfo
							Msg("ℹ️ GetUserInfo result for LID")
					}
				}
			}
		}
	}

	return MessageData{
		ID:            msg.Info.ID,
		From:          senderJID,
		To:            msg.Info.Chat.String(),
		Body:          body,
		Type:          msgType,
		Timestamp:     msg.Info.Timestamp.Unix(),
		FromMe:        msg.Info.IsFromMe,
		IsGroup:       msg.Info.IsGroup,
		PushName:      msg.Info.PushName,
		ResolvedPhone: resolvedPhone,
		MediaBase64:   mediaBase64,
		Mimetype:      mimetype,
		Caption:       caption,
		FileName:      fileName,
	}
}

// formatMessageLite formats a WhatsApp message WITHOUT downloading media
// Used for historical messages to avoid memory issues
func (m *Manager) formatMessageLite(instanceID string, msg *events.Message) MessageData {
	var body string
	var msgType string = "text"
	var mimetype string
	var caption string
	var fileName string

	// Check for different message types - but DON'T download media
	if msg.Message.GetConversation() != "" {
		body = msg.Message.GetConversation()
	} else if msg.Message.GetExtendedTextMessage() != nil {
		body = msg.Message.GetExtendedTextMessage().GetText()
	} else if imgMsg := msg.Message.GetImageMessage(); imgMsg != nil {
		msgType = "image"
		caption = imgMsg.GetCaption()
		mimetype = imgMsg.GetMimetype()
		body = caption
		// NO media download for history
	} else if vidMsg := msg.Message.GetVideoMessage(); vidMsg != nil {
		msgType = "video"
		caption = vidMsg.GetCaption()
		mimetype = vidMsg.GetMimetype()
		body = caption
	} else if audioMsg := msg.Message.GetAudioMessage(); audioMsg != nil {
		msgType = "audio"
		mimetype = audioMsg.GetMimetype()
	} else if docMsg := msg.Message.GetDocumentMessage(); docMsg != nil {
		msgType = "document"
		caption = docMsg.GetCaption()
		mimetype = docMsg.GetMimetype()
		fileName = docMsg.GetFileName()
		body = caption
	} else if stickerMsg := msg.Message.GetStickerMessage(); stickerMsg != nil {
		msgType = "sticker"
		mimetype = stickerMsg.GetMimetype()
	}

	return MessageData{
		ID:        msg.Info.ID,
		From:      msg.Info.Sender.String(),
		To:        msg.Info.Chat.String(),
		Body:      body,
		Type:      msgType,
		Timestamp: msg.Info.Timestamp.Unix(),
		FromMe:    msg.Info.IsFromMe,
		IsGroup:   msg.Info.IsGroup,
		PushName:  msg.Info.PushName,
		Mimetype:  mimetype,
		Caption:   caption,
		FileName:  fileName,
		// MediaBase64 is intentionally empty - no download for history
	}
}

// GetContactInfo attempts to get contact information and resolve LID if applicable
func (m *Manager) GetContactInfo(instanceID, jidStr string) (*ResolvedContactInfo, error) {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return nil, fmt.Errorf("instance not found")
	}

	inst.mu.RLock()
	status := inst.Status
	client := inst.Client
	inst.mu.RUnlock()

	if status != "connected" || client == nil {
		return nil, fmt.Errorf("instance not connected")
	}

	// Parse JID
	jid, err := types.ParseJID(jidStr)
	if err != nil {
		return nil, fmt.Errorf("invalid JID: %w", err)
	}

	result := &ResolvedContactInfo{
		OriginalJID: jidStr,
		IsLID:       strings.HasSuffix(jidStr, "@lid"),
		Resolved:    false,
	}

	// Try to get contact info from store
	if client.Store != nil && client.Store.Contacts != nil {
		contact, err := client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil {
			result.FullName = contact.FullName
			result.PushName = contact.PushName
		}
	}

	// If it's a LID, try to resolve to phone number
	if result.IsLID && client.Store != nil && client.Store.LIDs != nil {
		pnJID, err := client.Store.LIDs.GetPNForLID(context.Background(), jid)
		if err == nil && pnJID.User != "" {
			result.ResolvedPhone = pnJID.User
			result.Resolved = true
			log.Info().Str("lid", jidStr).Str("phone", result.ResolvedPhone).Msg("Successfully resolved LID to phone")
		} else {
			log.Debug().Str("lid", jidStr).Msg("Could not resolve LID - WhatsApp privacy restriction")
		}
	} else if !result.IsLID {
		// For regular JIDs, extract phone number directly
		result.ResolvedPhone = jid.User
		result.Resolved = true
	}

	return result, nil
}

// Connect connects an instance to WhatsApp
func (m *Manager) Connect(instanceID string) (*Instance, error) {
	inst, err := m.GetOrCreateInstance(instanceID)
	if err != nil {
		return nil, err
	}

	inst.mu.Lock()
	currentStatus := inst.Status
	inst.mu.Unlock()

	if currentStatus == "connected" {
		return inst, nil
	}

	inst.mu.Lock()
	inst.Status = "connecting"
	inst.mu.Unlock()

	// Check if already logged in
	if inst.Client.Store.ID != nil {
		// Already has session, try to connect
		err = inst.Client.Connect()
		if err != nil {
			inst.mu.Lock()
			inst.Status = "disconnected"
			inst.mu.Unlock()
			return nil, fmt.Errorf("failed to connect: %w", err)
		}
	} else {
		// No session, need QR code
		err = inst.Client.Connect()
		if err != nil {
			inst.mu.Lock()
			inst.Status = "disconnected"
			inst.mu.Unlock()
			return nil, fmt.Errorf("failed to connect: %w", err)
		}
	}

	return inst, nil
}

// ConnectWithPairingCode connects an instance using phone pairing code
func (m *Manager) ConnectWithPairingCode(instanceID, phoneNumber string) (string, error) {
	inst, err := m.GetOrCreateInstance(instanceID)
	if err != nil {
		return "", err
	}

	inst.mu.Lock()
	currentStatus := inst.Status
	inst.mu.Unlock()

	if currentStatus == "connected" {
		return "", fmt.Errorf("already connected")
	}

	// Check if already has a session - pairing code only works for new connections
	if inst.Client.Store.ID != nil {
		return "", fmt.Errorf("already has a session, use QR code or disconnect first")
	}

	// Clean phone number - remove + and any spaces/dashes
	phoneNumber = strings.TrimPrefix(phoneNumber, "+")
	phoneNumber = strings.ReplaceAll(phoneNumber, " ", "")
	phoneNumber = strings.ReplaceAll(phoneNumber, "-", "")

	log.Info().Str("instanceId", instanceID).Str("phone", phoneNumber).Msg("Starting pairing code connection")

	inst.mu.Lock()
	inst.Status = "pairing"
	inst.mu.Unlock()

	// Connect first (required before PairPhone)
	if !inst.Client.IsConnected() {
		log.Info().Str("instanceId", instanceID).Msg("Connecting to WhatsApp servers...")
		err = inst.Client.Connect()
		if err != nil {
			inst.mu.Lock()
			inst.Status = "disconnected"
			inst.mu.Unlock()
			log.Error().Err(err).Str("instanceId", instanceID).Msg("Failed to connect to WhatsApp servers")
			return "", fmt.Errorf("failed to connect: %w", err)
		}
	}

	// Wait a bit for connection to stabilize
	time.Sleep(2 * time.Second)

	// Check if connected
	if !inst.Client.IsConnected() {
		inst.mu.Lock()
		inst.Status = "disconnected"
		inst.mu.Unlock()
		return "", fmt.Errorf("failed to establish connection to WhatsApp servers")
	}

	log.Info().Str("instanceId", instanceID).Msg("Connected, requesting pairing code...")

	// Request pairing code
	code, err := inst.Client.PairPhone(context.Background(), phoneNumber, true, whatsmeow.PairClientChrome, "Chrome (Mac OS)")
	if err != nil {
		log.Error().Err(err).Str("instanceId", instanceID).Str("phone", phoneNumber).Msg("Failed to get pairing code")
		inst.mu.Lock()
		inst.Status = "disconnected"
		inst.mu.Unlock()
		return "", fmt.Errorf("failed to get pairing code: %w", err)
	}

	// Format code as XXXX-XXXX
	formattedCode := code
	if len(code) == 8 {
		formattedCode = code[:4] + "-" + code[4:]
	}

	inst.mu.Lock()
	inst.PairingCode = formattedCode
	inst.mu.Unlock()

	log.Info().Str("instanceId", instanceID).Str("code", formattedCode).Msg("Pairing code generated successfully")

	// Publish event
	m.publishEvent(Event{
		Type:       "pairing_code",
		InstanceID: instanceID,
		Data: map[string]string{
			"code": formattedCode,
		},
	})

	return formattedCode, nil
}

// GetPairingCode gets pairing code for instance
func (m *Manager) GetPairingCode(instanceID string) string {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return ""
	}

	inst.mu.RLock()
	defer inst.mu.RUnlock()

	return inst.PairingCode
}

// MarkChatAsRead marks a chat as read
func (m *Manager) MarkChatAsRead(instanceID, chatID string, messageIDs []string) error {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return fmt.Errorf("instance not found")
	}
	inst.mu.RLock()
	client := inst.Client
	inst.mu.RUnlock()

	if client == nil {
		return fmt.Errorf("client not initialized")
	}

	// Clean and parse chat JID
	chatID = strings.TrimPrefix(chatID, "+")
	chatID = strings.ReplaceAll(chatID, " ", "")
	chatID = strings.ReplaceAll(chatID, "-", "")

	if !strings.Contains(chatID, "@") {
		chatID = chatID + "@s.whatsapp.net"
	}

	chatJID, err := types.ParseJID(chatID)
	if err != nil {
		return fmt.Errorf("invalid chat JID: %w", err)
	}

	// Convert string IDs to MessageID type
	var msgIDs []types.MessageID
	for _, id := range messageIDs {
		msgIDs = append(msgIDs, types.MessageID(id))
	}

	// If no message IDs provided, we need at least one
	// Use a placeholder that whatsmeow might accept or return error
	if len(msgIDs) == 0 {
		return fmt.Errorf("at least one messageId is required to mark chat as read")
	}

	log.Info().
		Str("instanceId", instanceID).
		Str("chatJID", chatJID.String()).
		Int("messageCount", len(msgIDs)).
		Msg("Marking messages as read")

	// Mark as read
	return client.MarkRead(context.Background(), msgIDs, time.Now(), chatJID, types.EmptyJID)
}

// Disconnect disconnects an instance
func (m *Manager) Disconnect(instanceID string) error {
	m.mu.RLock()
	inst, ok := m.instances[instanceID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("instance %s not found", instanceID)
	}

	inst.Client.Disconnect()

	inst.mu.Lock()
	inst.Status = "disconnected"
	inst.mu.Unlock()

	return nil
}

// Logout logs out and removes session
func (m *Manager) Logout(instanceID string) error {
	m.mu.Lock()
	inst, ok := m.instances[instanceID]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("instance %s not found", instanceID)
	}

	err := inst.Client.Logout(context.Background())
	if err != nil {
		return fmt.Errorf("failed to logout: %w", err)
	}

	inst.Client.Disconnect()

	// Remove from memory
	m.mu.Lock()
	delete(m.instances, instanceID)
	m.mu.Unlock()

	return nil
}

// DisconnectAll disconnects all instances
func (m *Manager) DisconnectAll() {
	m.mu.RLock()
	instances := make([]*Instance, 0, len(m.instances))
	for _, inst := range m.instances {
		instances = append(instances, inst)
	}
	m.mu.RUnlock()

	for _, inst := range instances {
		inst.Client.Disconnect()
	}
}

// GetInstance gets an instance by ID
func (m *Manager) GetInstance(instanceID string) (*Instance, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inst, ok := m.instances[instanceID]
	return inst, ok
}

// GetStatus gets instance status
func (m *Manager) GetStatus(instanceID string) (string, map[string]string) {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return "not_found", nil
	}

	inst.mu.RLock()
	defer inst.mu.RUnlock()

	return inst.Status, map[string]string{
		"waNumber": inst.WANumber,
		"waName":   inst.WAName,
	}
}

// GetQRCode gets QR code for instance
func (m *Manager) GetQRCode(instanceID string) (string, string) {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return "", ""
	}

	inst.mu.RLock()
	defer inst.mu.RUnlock()

	return inst.QRCode, inst.QRCodeBase64
}

// LinkPreview holds Open Graph metadata for a URL
type LinkPreview struct {
	URL         string
	Title       string
	Description string
	SiteName    string
	ImageURL    string
	Thumbnail   []byte
}

// urlRegex matches http/https URLs
var urlRegex = regexp.MustCompile(`https?://[^\s<>"']+`)

// extractFirstURL finds the first URL in text
func extractFirstURL(text string) string {
	match := urlRegex.FindString(text)
	return match
}

// fetchLinkPreview fetches Open Graph metadata from a URL
func fetchLinkPreview(targetURL string) (*LinkPreview, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; WhatsApp/2.23; +http://www.whatsapp.com)")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	// Read body (limit to 1MB)
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return nil, err
	}

	htmlStr := string(body)

	preview := &LinkPreview{
		URL: targetURL,
	}

	// Extract Open Graph tags
	preview.Title = extractMetaContent(htmlStr, "og:title")
	if preview.Title == "" {
		preview.Title = extractHTMLTitle(htmlStr)
	}
	preview.Description = extractMetaContent(htmlStr, "og:description")
	if preview.Description == "" {
		preview.Description = extractMetaContent(htmlStr, "description")
	}
	preview.SiteName = extractMetaContent(htmlStr, "og:site_name")
	preview.ImageURL = extractMetaContent(htmlStr, "og:image")

	// Make image URL absolute if relative
	if preview.ImageURL != "" && !strings.HasPrefix(preview.ImageURL, "http") {
		baseURL, _ := url.Parse(targetURL)
		imgURL, _ := url.Parse(preview.ImageURL)
		preview.ImageURL = baseURL.ResolveReference(imgURL).String()
	}

	// Download thumbnail if available
	if preview.ImageURL != "" {
		preview.Thumbnail = downloadThumbnail(preview.ImageURL)
	}

	return preview, nil
}

// extractMetaContent extracts content from <meta property="name" content="value"> or <meta name="name" content="value">
func extractMetaContent(html, name string) string {
	// Try property="name"
	pattern := regexp.MustCompile(`<meta[^>]+(?:property|name)=["']` + regexp.QuoteMeta(name) + `["'][^>]+content=["']([^"']*)["']`)
	match := pattern.FindStringSubmatch(html)
	if len(match) > 1 {
		return match[1]
	}

	// Try content first
	pattern2 := regexp.MustCompile(`<meta[^>]+content=["']([^"']*)["'][^>]+(?:property|name)=["']` + regexp.QuoteMeta(name) + `["']`)
	match2 := pattern2.FindStringSubmatch(html)
	if len(match2) > 1 {
		return match2[1]
	}

	return ""
}

// extractHTMLTitle extracts <title> content
func extractHTMLTitle(html string) string {
	pattern := regexp.MustCompile(`<title[^>]*>([^<]*)</title>`)
	match := pattern.FindStringSubmatch(html)
	if len(match) > 1 {
		return strings.TrimSpace(match[1])
	}
	return ""
}

// downloadThumbnail downloads and returns image bytes (limited size)
func downloadThumbnail(imageURL string) []byte {
	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	req, err := http.NewRequest("GET", imageURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	// Limit to 500KB
	data, err := io.ReadAll(io.LimitReader(resp.Body, 500*1024))
	if err != nil {
		return nil
	}

	return data
}

// SendTextMessage sends a text message (with automatic link preview if URL detected)
func (m *Manager) SendTextMessage(instanceID, to, text string) (string, error) {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return "", fmt.Errorf("instance %s not found", instanceID)
	}

	inst.mu.RLock()
	status := inst.Status
	inst.mu.RUnlock()

	if status != "connected" {
		return "", fmt.Errorf("instance not connected (status: %s)", status)
	}

	// Parse recipient JID
	// Ensure the number is just digits
	to = strings.TrimPrefix(to, "+")

	// First, check if the user is on WhatsApp to get the correct JID
	users, err := inst.Client.IsOnWhatsApp(context.Background(), []string{to})
	if err != nil {
		log.Error().Err(err).Str("instanceId", instanceID).Str("to", to).Msg("Failed to check if user is on WhatsApp")
		return "", fmt.Errorf("failed to check if user is on WhatsApp: %w", err)
	}

	// IsOnWhatsApp returns a list of contacts. If the number is not registered, it might return a contact with VerifiedName nil or similar,
	// but usually checking if JID is present is enough.
	if len(users) == 0 {
		return "", fmt.Errorf("user %s not on WhatsApp", to)
	}

	if users[0].JID.User == "" {
		return "", fmt.Errorf("received empty JID for user %s", to)
	}

	// Use the correct JID returned by server
	jid := users[0].JID

	// Build message - check for URLs to generate preview
	var msg *waE2E.Message

	foundURL := extractFirstURL(text)
	if foundURL != "" {
		log.Debug().Str("instanceId", instanceID).Str("url", foundURL).Msg("URL detected, fetching link preview")

		// Try to fetch link preview (don't fail if it doesn't work)
		preview, err := fetchLinkPreview(foundURL)
		if err != nil {
			log.Warn().Err(err).Str("url", foundURL).Msg("Failed to fetch link preview, sending as plain text")
			// Fall back to plain text
			msg = &waE2E.Message{
				Conversation: proto.String(text),
			}
		} else {
			log.Info().Str("instanceId", instanceID).Str("title", preview.Title).Str("url", foundURL).Msg("Link preview fetched successfully")

			// Build ExtendedTextMessage with preview
			extMsg := &waE2E.ExtendedTextMessage{
				Text:        proto.String(text),
				MatchedText: proto.String(foundURL),
				PreviewType: waE2E.ExtendedTextMessage_VIDEO.Enum(), // Use VIDEO type for rich preview
			}

			if preview.Title != "" {
				extMsg.Title = proto.String(preview.Title)
			}
			if preview.Description != "" {
				extMsg.Description = proto.String(preview.Description)
			}
			if len(preview.Thumbnail) > 0 {
				extMsg.JPEGThumbnail = preview.Thumbnail
			}

			msg = &waE2E.Message{
				ExtendedTextMessage: extMsg,
			}
		}
	} else {
		// No URL, send as plain conversation
		msg = &waE2E.Message{
			Conversation: proto.String(text),
		}
	}

	log.Debug().Str("instanceId", instanceID).Str("jid", jid.String()).Msg("Attempting to send message via whatsmeow")

	resp, err := inst.Client.SendMessage(context.Background(), jid, msg)
	if err != nil {
		log.Error().Err(err).Str("instanceId", instanceID).Str("jid", jid.String()).Msg("Whatsmeow SendMessage failed")
		return "", fmt.Errorf("whatsmeow send error: %w", err)
	}

	// Clear presence (stop typing) immediately after sending
	go func() {
		inst.Client.SendChatPresence(context.Background(), jid, types.ChatPresencePaused, types.ChatPresenceMediaText)
	}()

	log.Info().Str("instanceId", instanceID).Str("msgId", resp.ID).Msg("Message sent successfully")
	return resp.ID, nil
}

// SendPresence sends presence (composing, recording, paused)
func (m *Manager) SendPresence(instanceID, to, presence string) error {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return fmt.Errorf("instance %s not found", instanceID)
	}

	inst.mu.RLock()
	status := inst.Status
	inst.mu.RUnlock()

	if status != "connected" {
		return fmt.Errorf("instance not connected")
	}

	// Clean number
	to = strings.TrimPrefix(to, "+")

	// Start verification
	users, err := inst.Client.IsOnWhatsApp(context.Background(), []string{to})
	if err != nil {
		return fmt.Errorf("failed to check user: %w", err)
	}
	if len(users) == 0 {
		return fmt.Errorf("user %s not on WhatsApp", to)
	}

	jid := users[0].JID

	// logic above specifically sends chat presence (typing...),
	// standard presence (online) is handled differently but usually automatic.
	// We'll stick to ChatPresence for "typing" indicators as requested by "Presença" button usually.

	var p types.ChatPresence
	var mp types.ChatPresenceMedia

	switch presence {
	case "composing":
		p = types.ChatPresenceComposing
		mp = types.ChatPresenceMediaText
	case "recording":
		p = types.ChatPresenceComposing
		mp = types.ChatPresenceMediaAudio
	case "paused":
		p = types.ChatPresencePaused
		mp = types.ChatPresenceMediaText
	default:
		p = types.ChatPresenceComposing
		mp = types.ChatPresenceMediaText
	}

	err = inst.Client.SendChatPresence(context.Background(), jid, p, mp)
	if err != nil {
		return fmt.Errorf("failed to send presence: %w", err)
	}

	return nil
}

// SendMediaMessage sends a media message (image, video, audio, document)
func (m *Manager) SendMediaMessage(instanceID, to, mediaUrl, caption, mediaType string) (string, error) {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return "", fmt.Errorf("instance %s not found", instanceID)
	}

	// Clean number and verify
	to = strings.TrimPrefix(to, "+")
	users, err := inst.Client.IsOnWhatsApp(context.Background(), []string{to})
	if err != nil || len(users) == 0 {
		return "", fmt.Errorf("user %s not on WhatsApp", to)
	}
	jid := users[0].JID

	var data []byte
	var mimeType string

	if strings.HasPrefix(mediaUrl, "data:") {
		// Handle Data URI
		parts := strings.SplitN(mediaUrl, ",", 2)
		if len(parts) != 2 {
			return "", fmt.Errorf("invalid data URI")
		}
		// Extract mime
		meta := strings.SplitN(parts[0], ";", 2)
		if len(meta) > 0 {
			mimeType = strings.TrimPrefix(meta[0], "data:")
		}

		// Decode
		var decodeErr error
		if strings.Contains(parts[0], ";base64") {
			data, decodeErr = base64.StdEncoding.DecodeString(parts[1])
		} else {
			// URL encoded
			return "", fmt.Errorf("url-encoded data URIs not supported yet")
		}
		if decodeErr != nil {
			return "", fmt.Errorf("failed to decode data URI: %w", decodeErr)
		}
	} else {
		// Handle URL
		req, err := http.NewRequest("GET", mediaUrl, nil)
		if err != nil {
			return "", fmt.Errorf("failed to create request: %w", err)
		}

		// Add User-Agent to avoid 403 Forbidden on some servers
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

		transport := &http.Transport{
			DisableKeepAlives: true,
		}
		client := &http.Client{
			Timeout:   30 * time.Second,
			Transport: transport,
		}

		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("failed to download media: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return "", fmt.Errorf("failed to download media, status: %d", resp.StatusCode)
		}

		data, err = io.ReadAll(resp.Body)
		if err != nil {
			return "", fmt.Errorf("failed to read media body: %w", err)
		}
		mimeType = http.DetectContentType(data)
	}

	log.Info().Str("instanceId", instanceID).Str("mediaType", mediaType).Str("mimeType", mimeType).Msg("Uploading media")

	// Determine upload type based on mediaType or mimeType
	var appMedia whatsmeow.MediaType
	switch mediaType {
	case "image":
		appMedia = whatsmeow.MediaImage
	case "video":
		appMedia = whatsmeow.MediaVideo
	case "audio":
		appMedia = whatsmeow.MediaAudio
	default:
		// Infer from mime
		if strings.HasPrefix(mimeType, "image/") {
			appMedia = whatsmeow.MediaImage
			mediaType = "image"
		} else if strings.HasPrefix(mimeType, "video/") {
			appMedia = whatsmeow.MediaVideo
			mediaType = "video"
		} else if strings.HasPrefix(mimeType, "audio/") {
			appMedia = whatsmeow.MediaAudio
			mediaType = "audio"
		} else {
			appMedia = whatsmeow.MediaDocument
			mediaType = "document"
		}
	}

	// Upload to WhatsApp
	uploaded, err := inst.Client.Upload(context.Background(), data, appMedia)
	if err != nil {
		return "", fmt.Errorf("failed to upload media: %w", err)
	}

	msg := &waE2E.Message{}

	switch mediaType {
	case "image":
		msg.ImageMessage = &waE2E.ImageMessage{
			Caption:       proto.String(caption),
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			Mimetype:      proto.String(mimeType),
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
		}
	case "video":
		msg.VideoMessage = &waE2E.VideoMessage{
			Caption:       proto.String(caption),
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			Mimetype:      proto.String(mimeType),
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
		}
	case "audio":
		msg.AudioMessage = &waE2E.AudioMessage{
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			Mimetype:      proto.String(mimeType),
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
			PTT:           proto.Bool(true),
		}
	case "document":
		msg.DocumentMessage = &waE2E.DocumentMessage{
			Caption:       proto.String(caption),
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			Mimetype:      proto.String(mimeType),
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uint64(len(data))),
			FileName:      proto.String("file"), // TODO: Parse filename from URL
		}
	default:
		return "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	sentResp, err := inst.Client.SendMessage(context.Background(), jid, msg)
	if err != nil {
		return "", fmt.Errorf("failed to send media message: %w", err)
	}

	// Clear presence (stop typing/recording) immediately after sending
	go func() {
		inst.Client.SendChatPresence(context.Background(), jid, types.ChatPresencePaused, types.ChatPresenceMediaText)
	}()

	return sentResp.ID, nil
}

// SendLocationMessage sends a location message
func (m *Manager) SendLocationMessage(instanceID, to string, latitude, longitude float64, description string) (string, error) {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return "", fmt.Errorf("instance not found")
	}

	inst.mu.RLock()
	status := inst.Status
	inst.mu.RUnlock()

	if status != "connected" {
		return "", fmt.Errorf("instance not connected")
	}

	// Clean phone number
	to = strings.TrimPrefix(to, "+")
	to = strings.ReplaceAll(to, " ", "")
	to = strings.ReplaceAll(to, "-", "")

	// Ensure it has @s.whatsapp.net suffix
	if !strings.Contains(to, "@") {
		to = to + "@s.whatsapp.net"
	}

	jid, err := types.ParseJID(to)
	if err != nil {
		return "", fmt.Errorf("invalid JID: %w", err)
	}

	msg := &waE2E.Message{
		LocationMessage: &waE2E.LocationMessage{
			DegreesLatitude:  proto.Float64(latitude),
			DegreesLongitude: proto.Float64(longitude),
			Name:             proto.String(description),
			Address:          proto.String(description),
		},
	}

	log.Info().
		Str("instanceId", instanceID).
		Str("to", to).
		Float64("lat", latitude).
		Float64("long", longitude).
		Msg("Sending location message")

	sentResp, err := inst.Client.SendMessage(context.Background(), jid, msg)
	if err != nil {
		return "", fmt.Errorf("failed to send location: %w", err)
	}

	return sentResp.ID, nil
}

// SendPollMessage sends a poll message
func (m *Manager) SendPollMessage(instanceID, to, question string, options []string, selectableCount int) (string, error) {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return "", fmt.Errorf("instance not found")
	}

	inst.mu.RLock()
	status := inst.Status
	inst.mu.RUnlock()

	if status != "connected" {
		return "", fmt.Errorf("instance not connected")
	}

	// Clean phone number
	to = strings.TrimPrefix(to, "+")
	to = strings.ReplaceAll(to, " ", "")
	to = strings.ReplaceAll(to, "-", "")

	// Ensure it has @s.whatsapp.net suffix
	if !strings.Contains(to, "@") {
		to = to + "@s.whatsapp.net"
	}

	jid, err := types.ParseJID(to)
	if err != nil {
		return "", fmt.Errorf("invalid JID: %w", err)
	}

	// Resolve user devices first to ensure LID is available
	// This is required for polls to work in the new multi-device architecture
	_, err = inst.Client.GetUserDevices(context.Background(), []types.JID{jid})
	if err != nil {
		log.Warn().Err(err).Str("to", to).Msg("Failed to get user devices, trying to send anyway")
	}

	// Create poll message
	pollMsg := inst.Client.BuildPollCreation(question, options, selectableCount)

	log.Info().
		Str("instanceId", instanceID).
		Str("to", to).
		Str("question", question).
		Int("options", len(options)).
		Msg("Sending poll message")

	sentResp, err := inst.Client.SendMessage(context.Background(), jid, pollMsg)
	if err != nil {
		return "", fmt.Errorf("failed to send poll: %w", err)
	}

	return sentResp.ID, nil
}

// EditMessage edits a previously sent message
func (m *Manager) EditMessage(instanceID, chatID, messageID, newText string) (string, error) {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return "", fmt.Errorf("instance not found")
	}

	inst.mu.RLock()
	status := inst.Status
	inst.mu.RUnlock()

	if status != "connected" {
		return "", fmt.Errorf("instance not connected")
	}

	// Clean phone number / chat ID
	chatID = strings.TrimPrefix(chatID, "+")
	chatID = strings.ReplaceAll(chatID, " ", "")
	chatID = strings.ReplaceAll(chatID, "-", "")

	if !strings.Contains(chatID, "@") {
		chatID = chatID + "@s.whatsapp.net"
	}

	chatJID, err := types.ParseJID(chatID)
	if err != nil {
		return "", fmt.Errorf("invalid chat JID: %w", err)
	}

	// Use IsOnWhatsApp to resolve LID - this queries the server and populates PN→LID mapping
	isOnWA, err := inst.Client.IsOnWhatsApp(context.Background(), []string{strings.TrimSuffix(chatID, "@s.whatsapp.net")})
	if err != nil {
		log.Warn().Err(err).Str("chatId", chatID).Msg("Failed to check IsOnWhatsApp, trying to send anyway")
	} else if len(isOnWA) > 0 && isOnWA[0].IsIn {
		// Use the resolved JID from the server
		chatJID = isOnWA[0].JID
		log.Info().Str("resolvedJID", chatJID.String()).Msg("Using resolved WhatsApp JID for edit")
	}

	// Build edit message
	log.Info().
		Str("instanceId", instanceID).
		Str("chatJID", chatJID.String()).
		Str("messageId", messageID).
		Str("newText", newText).
		Msg("Building edit message")

	editMsg := inst.Client.BuildEdit(chatJID, messageID, &waE2E.Message{
		Conversation: proto.String(newText),
	})

	log.Info().
		Str("instanceId", instanceID).
		Str("chatId", chatID).
		Str("messageId", messageID).
		Msg("Sending edited message")

	sentResp, err := inst.Client.SendMessage(context.Background(), chatJID, editMsg)
	if err != nil {
		log.Error().
			Err(err).
			Str("instanceId", instanceID).
			Str("chatJID", chatJID.String()).
			Str("messageId", messageID).
			Msg("Failed to send edited message")
		return "", fmt.Errorf("failed to edit message: %w", err)
	}

	return sentResp.ID, nil
}

// ReactToMessage sends a reaction to a message
func (m *Manager) ReactToMessage(instanceID, chatID, messageID, reaction string) error {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return fmt.Errorf("instance not found")
	}

	inst.mu.RLock()
	status := inst.Status
	inst.mu.RUnlock()

	if status != "connected" {
		return fmt.Errorf("instance not connected")
	}

	// Clean phone number / chat ID
	chatID = strings.TrimPrefix(chatID, "+")
	chatID = strings.ReplaceAll(chatID, " ", "")
	chatID = strings.ReplaceAll(chatID, "-", "")

	if !strings.Contains(chatID, "@") {
		chatID = chatID + "@s.whatsapp.net"
	}

	chatJID, err := types.ParseJID(chatID)
	if err != nil {
		return fmt.Errorf("invalid chat JID: %w", err)
	}

	// Use IsOnWhatsApp to resolve LID - this queries the server and populates PN→LID mapping
	isOnWA, err := inst.Client.IsOnWhatsApp(context.Background(), []string{strings.TrimSuffix(chatID, "@s.whatsapp.net")})
	if err != nil {
		log.Warn().Err(err).Str("chatId", chatID).Msg("Failed to check IsOnWhatsApp, trying to send anyway")
	} else if len(isOnWA) > 0 && isOnWA[0].IsIn {
		// Use the resolved JID from the server
		chatJID = isOnWA[0].JID
		log.Info().Str("resolvedJID", chatJID.String()).Msg("Using resolved WhatsApp JID for reaction")
	}

	log.Info().
		Str("instanceId", instanceID).
		Str("chatJID", chatJID.String()).
		Str("messageId", messageID).
		Str("reaction", reaction).
		Msg("Building and sending reaction")

	// Build reaction using whatsmeow's method
	reactionMsg := inst.Client.BuildReaction(chatJID, types.EmptyJID, messageID, reaction)
	_, err = inst.Client.SendMessage(context.Background(), chatJID, reactionMsg)
	if err != nil {
		log.Error().
			Err(err).
			Str("instanceId", instanceID).
			Str("chatJID", chatJID.String()).
			Str("messageId", messageID).
			Str("reaction", reaction).
			Msg("Failed to send reaction")
		return fmt.Errorf("failed to send reaction: %w", err)
	}

	return nil
}

// DeleteMessage deletes a message (revoke)
func (m *Manager) DeleteMessage(instanceID, chatID, messageID string, forEveryone bool) error {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return fmt.Errorf("instance not found")
	}

	inst.mu.RLock()
	status := inst.Status
	inst.mu.RUnlock()

	if status != "connected" {
		return fmt.Errorf("instance not connected")
	}

	// Clean phone number / chat ID
	chatID = strings.TrimPrefix(chatID, "+")
	chatID = strings.ReplaceAll(chatID, " ", "")
	chatID = strings.ReplaceAll(chatID, "-", "")

	if !strings.Contains(chatID, "@") {
		chatID = chatID + "@s.whatsapp.net"
	}

	chatJID, err := types.ParseJID(chatID)
	if err != nil {
		return fmt.Errorf("invalid chat JID: %w", err)
	}

	// Use IsOnWhatsApp to resolve LID - this queries the server and populates PN→LID mapping
	isOnWA, err := inst.Client.IsOnWhatsApp(context.Background(), []string{strings.TrimSuffix(chatID, "@s.whatsapp.net")})
	if err != nil {
		log.Warn().Err(err).Str("chatId", chatID).Msg("Failed to check IsOnWhatsApp, trying to send anyway")
	} else if len(isOnWA) > 0 && isOnWA[0].IsIn {
		log.Info().Str("jid", isOnWA[0].JID.String()).Msg("Resolved WhatsApp JID for delete")
	}

	log.Info().
		Str("instanceId", instanceID).
		Str("chatId", chatID).
		Str("messageId", messageID).
		Bool("forEveryone", forEveryone).
		Msg("Deleting message")

	if forEveryone {
		// Revoke for everyone
		revokeMsg := inst.Client.BuildRevoke(chatJID, types.EmptyJID, messageID)
		_, err = inst.Client.SendMessage(context.Background(), chatJID, revokeMsg)
	} else {
		// Delete for me only - uses a different method
		_, err = inst.Client.SendMessage(context.Background(), chatJID, inst.Client.BuildRevoke(chatJID, inst.Client.Store.ID.ToNonAD(), messageID))
	}

	if err != nil {
		return fmt.Errorf("failed to delete message: %w", err)
	}

	return nil
}

// Subscribe to events for an instance
func (m *Manager) Subscribe(instanceID string) chan Event {
	m.eventSubsMu.Lock()
	defer m.eventSubsMu.Unlock()

	ch := make(chan Event, 100)
	m.eventSubs[instanceID] = append(m.eventSubs[instanceID], ch)
	return ch
}

// Unsubscribe from events
func (m *Manager) Unsubscribe(instanceID string, ch chan Event) {
	m.eventSubsMu.Lock()
	defer m.eventSubsMu.Unlock()

	subs := m.eventSubs[instanceID]
	for i, sub := range subs {
		if sub == ch {
			m.eventSubs[instanceID] = append(subs[:i], subs[i+1:]...)
			close(ch)
			break
		}
	}
}

// publishEvent publishes event to all subscribers
func (m *Manager) publishEvent(evt Event) {
	if evt.Timestamp == 0 {
		evt.Timestamp = time.Now().Unix()
	}

	m.eventSubsMu.RLock()
	subs := m.eventSubs[evt.InstanceID]
	m.eventSubsMu.RUnlock()

	for _, ch := range subs {
		select {
		case ch <- evt:
		default:
			// Channel full, skip
		}
	}
}

// ChatInfo represents a chat/conversation
type ChatInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IsGroup  bool   `json:"isGroup"`
	PushName string `json:"pushName,omitempty"`
}

// ContactInfo represents a contact
type ContactInfo struct {
	JID      string `json:"jid"`
	Name     string `json:"name,omitempty"`
	PushName string `json:"pushName,omitempty"`
	Phone    string `json:"phone,omitempty"`
}

// GroupInfo represents a group
type GroupInfo struct {
	JID          string   `json:"jid"`
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	Participants []string `json:"participants,omitempty"`
}

// CheckNumberResult represents number check result
type CheckNumberResult struct {
	Number       string `json:"number"`
	IsOnWhatsApp bool   `json:"isOnWhatsApp"`
	JID          string `json:"jid,omitempty"`
}

// GetContacts gets all contacts for an instance
func (m *Manager) GetContacts(instanceID string) ([]ContactInfo, error) {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return nil, fmt.Errorf("instance not found")
	}

	inst.mu.RLock()
	status := inst.Status
	client := inst.Client
	inst.mu.RUnlock()

	if status != "connected" || client == nil {
		return nil, fmt.Errorf("instance not connected")
	}

	contacts := make([]ContactInfo, 0)

	// Get contacts from the store
	if client.Store != nil && client.Store.Contacts != nil {
		allContacts, err := client.Store.Contacts.GetAllContacts(context.Background())
		if err != nil {
			log.Warn().Err(err).Msg("Failed to get contacts from store")
		} else {
			for jid, contact := range allContacts {
				contacts = append(contacts, ContactInfo{
					JID:      jid.String(),
					Name:     contact.FullName,
					PushName: contact.PushName,
					Phone:    jid.User,
				})
			}
		}
	}

	return contacts, nil
}

// GetChats gets all chats/conversations for an instance
func (m *Manager) GetChats(instanceID string) ([]ChatInfo, error) {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return nil, fmt.Errorf("instance not found")
	}

	inst.mu.RLock()
	status := inst.Status
	client := inst.Client
	inst.mu.RUnlock()

	if status != "connected" || client == nil {
		return nil, fmt.Errorf("instance not connected")
	}

	chats := make([]ChatInfo, 0)

	// Get contacts from the store - these represent recent chats
	if client.Store != nil && client.Store.Contacts != nil {
		allContacts, err := client.Store.Contacts.GetAllContacts(context.Background())
		if err != nil {
			log.Warn().Err(err).Msg("Failed to get contacts from store")
		} else {
			for jid, contact := range allContacts {
				isGroup := jid.Server == "g.us"
				name := contact.FullName
				if name == "" {
					name = contact.PushName
				}
				if name == "" {
					name = jid.User
				}

				chats = append(chats, ChatInfo{
					ID:       jid.String(),
					Name:     name,
					IsGroup:  isGroup,
					PushName: contact.PushName,
				})
			}
		}
	}

	log.Info().Int("count", len(chats)).Str("instanceId", instanceID).Msg("Got chats")
	return chats, nil
}

// GetGroups gets all groups for an instance
func (m *Manager) GetGroups(instanceID string) ([]GroupInfo, error) {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return nil, fmt.Errorf("instance not found")
	}

	inst.mu.RLock()
	status := inst.Status
	client := inst.Client
	inst.mu.RUnlock()

	if status != "connected" || client == nil {
		return nil, fmt.Errorf("instance not connected")
	}

	groups := make([]GroupInfo, 0)

	// Get groups from joined groups
	joinedGroups, err := client.GetJoinedGroups(context.Background())
	if err != nil {
		log.Warn().Err(err).Msg("Failed to get joined groups")
	} else {
		for _, group := range joinedGroups {
			groups = append(groups, GroupInfo{
				JID:         group.JID.String(),
				Name:        group.Name,
				Description: group.Topic,
			})
		}
	}

	return groups, nil
}

// CheckNumber checks if a number is on WhatsApp
func (m *Manager) CheckNumber(instanceID, number string) (*CheckNumberResult, error) {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return nil, fmt.Errorf("instance not found")
	}

	inst.mu.RLock()
	status := inst.Status
	client := inst.Client
	inst.mu.RUnlock()

	if status != "connected" || client == nil {
		return nil, fmt.Errorf("instance not connected")
	}

	// Clean phone number
	number = strings.TrimPrefix(number, "+")
	number = strings.ReplaceAll(number, " ", "")
	number = strings.ReplaceAll(number, "-", "")

	result, err := client.IsOnWhatsApp(context.Background(), []string{number})
	if err != nil {
		return nil, fmt.Errorf("failed to check number: %w", err)
	}

	if len(result) == 0 {
		return &CheckNumberResult{
			Number:       number,
			IsOnWhatsApp: false,
		}, nil
	}

	return &CheckNumberResult{
		Number:       number,
		IsOnWhatsApp: result[0].IsIn,
		JID:          result[0].JID.String(),
	}, nil
}

// storeMessage stores a message in memory for later retrieval
func (m *Manager) storeMessage(instanceID, chatID string, msg MessageData) {
	m.messagesMu.Lock()
	defer m.messagesMu.Unlock()

	if m.messages[instanceID] == nil {
		m.messages[instanceID] = make(map[string][]MessageData)
	}

	// Limit to last 500 messages per chat to avoid memory issues
	msgs := m.messages[instanceID][chatID]
	msgs = append(msgs, msg)
	if len(msgs) > 500 {
		msgs = msgs[len(msgs)-500:]
	}
	m.messages[instanceID][chatID] = msgs
}

// GetChatMessages returns stored messages for a specific chat
func (m *Manager) GetChatMessages(instanceID, chatID string, limit int) ([]MessageData, error) {
	m.messagesMu.RLock()
	defer m.messagesMu.RUnlock()

	if m.messages[instanceID] == nil {
		return []MessageData{}, nil
	}

	msgs := m.messages[instanceID][chatID]
	if msgs == nil {
		return []MessageData{}, nil
	}

	// Return last N messages
	if limit > 0 && len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}

	return msgs, nil
}

// GetAllStoredChats returns list of chats that have stored messages
func (m *Manager) GetAllStoredChats(instanceID string) []string {
	m.messagesMu.RLock()
	defer m.messagesMu.RUnlock()

	if m.messages[instanceID] == nil {
		return []string{}
	}

	chats := make([]string, 0, len(m.messages[instanceID]))
	for chatID := range m.messages[instanceID] {
		chats = append(chats, chatID)
	}
	return chats
}

// SetRejectCalls sets the reject calls setting for an instance
func (m *Manager) SetRejectCalls(instanceID string, value bool) {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return
	}
	inst.mu.Lock()
	inst.RejectCalls = value
	inst.mu.Unlock()
	log.Info().Str("instanceId", instanceID).Bool("rejectCalls", value).Msg("Updated reject calls setting")
}

// SetAlwaysOnline sets the always online setting for an instance
func (m *Manager) SetAlwaysOnline(instanceID string, value bool) {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return
	}
	inst.mu.Lock()
	inst.AlwaysOnline = value
	inst.mu.Unlock()
	log.Info().Str("instanceId", instanceID).Bool("alwaysOnline", value).Msg("Updated always online setting")

	// If enabled and connected, send presence
	if value && inst.Client != nil && inst.Status == "connected" {
		inst.Client.SendPresence(context.Background(), types.PresenceAvailable)
	}
}

// SetIgnoreGroups sets the ignore groups setting for an instance
func (m *Manager) SetIgnoreGroups(instanceID string, value bool) {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return
	}
	inst.mu.Lock()
	inst.IgnoreGroups = value
	inst.mu.Unlock()
	log.Info().Str("instanceId", instanceID).Bool("ignoreGroups", value).Msg("Updated ignore groups setting")
}

// SetReadMessages sets the auto read messages setting for an instance
func (m *Manager) SetReadMessages(instanceID string, value bool) {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return
	}
	inst.mu.Lock()
	inst.ReadMessages = value
	inst.mu.Unlock()
	log.Info().Str("instanceId", instanceID).Bool("readMessages", value).Msg("Updated read messages setting")
}

// GetSettings returns the current settings for an instance
func (m *Manager) GetSettings(instanceID string) map[string]bool {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return map[string]bool{}
	}
	inst.mu.RLock()
	defer inst.mu.RUnlock()
	return map[string]bool{
		"rejectCalls":  inst.RejectCalls,
		"alwaysOnline": inst.AlwaysOnline,
		"ignoreGroups": inst.IgnoreGroups,
		"readMessages": inst.ReadMessages,
	}
}

// SetProxy sets the proxy configuration for an instance
func (m *Manager) SetProxy(instanceID string, host, port, username, password, protocol string) error {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return fmt.Errorf("instance not found")
	}

	inst.mu.Lock()
	inst.ProxyHost = host
	inst.ProxyPort = port
	inst.ProxyUsername = username
	inst.ProxyPassword = password
	inst.ProxyProtocol = protocol
	client := inst.Client
	status := inst.Status
	inst.mu.Unlock()

	// Build proxy URL
	proxyURL := m.buildProxyURL(host, port, username, password, protocol)

	if client != nil {
		client.SetProxyAddress(proxyURL)
		if proxyURL != "" {
			log.Info().Str("instanceId", instanceID).Str("proxy", host+":"+port).Msg("Proxy configured")
		} else {
			log.Info().Str("instanceId", instanceID).Msg("Proxy disabled")
		}

		// If connected, reconnect to apply the new proxy
		if status == "connected" {
			log.Info().Str("instanceId", instanceID).Msg("Reconnecting to apply proxy...")
			go func() {
				// Disconnect and reconnect
				client.Disconnect()
				time.Sleep(1 * time.Second)
				if err := client.Connect(); err != nil {
					log.Error().Err(err).Str("instanceId", instanceID).Msg("Failed to reconnect after proxy change")
				} else {
					log.Info().Str("instanceId", instanceID).Msg("Reconnected with new proxy settings")
				}
			}()
		}
	}

	return nil
}

// buildProxyURL constructs the proxy URL from components
func (m *Manager) buildProxyURL(host, port, username, password, protocol string) string {
	if host == "" || port == "" {
		return ""
	}

	// Default to socks5 if not specified
	if protocol == "" {
		protocol = "socks5"
	}

	var proxyURL string
	if username != "" && password != "" {
		proxyURL = fmt.Sprintf("%s://%s:%s@%s:%s", protocol, username, password, host, port)
	} else {
		proxyURL = fmt.Sprintf("%s://%s:%s", protocol, host, port)
	}

	return proxyURL
}

// CheckProxyIP checks the external IP address using the instance's proxy configuration
func (m *Manager) CheckProxyIP(instanceID string) (string, error) {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return "", fmt.Errorf("instance not found")
	}

	// Get proxy settings
	inst.mu.RLock()
	host := inst.ProxyHost
	port := inst.ProxyPort
	username := inst.ProxyUsername
	password := inst.ProxyPassword
	protocol := inst.ProxyProtocol
	inst.mu.RUnlock()

	transport := &http.Transport{}

	// Configure proxy if set
	if host != "" && port != "" {
		proxyURLStr := m.buildProxyURL(host, port, username, password, protocol)
		parsedURL, err := url.Parse(proxyURLStr)
		if err != nil {
			return "", fmt.Errorf("invalid proxy URL: %w", err)
		}
		transport.Proxy = http.ProxyURL(parsedURL)
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}

	// Request to get public IP
	resp, err := client.Get("https://api.ipify.org")
	if err != nil {
		return "", fmt.Errorf("failed to check IP: %w", err)
	}
	defer resp.Body.Close()

	ipBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read IP response: %w", err)
	}

	return string(ipBytes), nil
}

// GetProxy returns the current proxy configuration for an instance
func (m *Manager) GetProxy(instanceID string) map[string]string {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return map[string]string{}
	}
	inst.mu.RLock()
	defer inst.mu.RUnlock()
	return map[string]string{
		"proxyHost":     inst.ProxyHost,
		"proxyPort":     inst.ProxyPort,
		"proxyUsername": inst.ProxyUsername,
		"proxyProtocol": inst.ProxyProtocol,
	}
}

// DownloadMediaRequest contains the info needed to download media
type DownloadMediaRequest struct {
	URL           string `json:"url"`
	DirectPath    string `json:"directPath"`
	MediaKey      []byte `json:"mediaKey"`
	FileEncSHA256 []byte `json:"fileEncSha256"`
	FileSHA256    []byte `json:"fileSha256"`
	FileLength    uint64 `json:"fileLength"`
	MediaType     string `json:"mediaType"` // image, video, audio, document
	Mimetype      string `json:"mimetype"`
}

// DownloadMedia downloads media from a WhatsApp message
func (m *Manager) DownloadMedia(instanceID string, mediaInfo DownloadMediaRequest) ([]byte, string, error) {
	inst, ok := m.GetInstance(instanceID)
	if !ok {
		return nil, "", fmt.Errorf("instance not found")
	}

	inst.mu.RLock()
	status := inst.Status
	client := inst.Client
	inst.mu.RUnlock()

	if status != "connected" || client == nil {
		return nil, "", fmt.Errorf("instance not connected")
	}

	// Determine media type for whatsmeow
	var mediaType whatsmeow.MediaType
	switch mediaInfo.MediaType {
	case "image":
		mediaType = whatsmeow.MediaImage
	case "video":
		mediaType = whatsmeow.MediaVideo
	case "audio":
		mediaType = whatsmeow.MediaAudio
	case "document":
		mediaType = whatsmeow.MediaDocument
	case "sticker":
		mediaType = whatsmeow.MediaImage // Stickers are images
	default:
		// Try to infer from mimetype
		if strings.HasPrefix(mediaInfo.Mimetype, "image/") {
			mediaType = whatsmeow.MediaImage
		} else if strings.HasPrefix(mediaInfo.Mimetype, "video/") {
			mediaType = whatsmeow.MediaVideo
		} else if strings.HasPrefix(mediaInfo.Mimetype, "audio/") {
			mediaType = whatsmeow.MediaAudio
		} else {
			mediaType = whatsmeow.MediaDocument
		}
	}

	log.Info().
		Str("instanceId", instanceID).
		Str("mediaType", mediaInfo.MediaType).
		Str("mimetype", mediaInfo.Mimetype).
		Uint64("fileLength", mediaInfo.FileLength).
		Msg("Downloading media")

	// Download using whatsmeow - the Download method accepts a DownloadableMessage
	// We create the appropriate message type based on what we're downloading
	var data []byte
	var err error

	switch mediaType {
	case whatsmeow.MediaImage:
		data, err = client.Download(context.Background(), &waE2E.ImageMessage{
			URL:           proto.String(mediaInfo.URL),
			DirectPath:    proto.String(mediaInfo.DirectPath),
			MediaKey:      mediaInfo.MediaKey,
			FileEncSHA256: mediaInfo.FileEncSHA256,
			FileSHA256:    mediaInfo.FileSHA256,
			FileLength:    proto.Uint64(mediaInfo.FileLength),
			Mimetype:      proto.String(mediaInfo.Mimetype),
		})
	case whatsmeow.MediaVideo:
		data, err = client.Download(context.Background(), &waE2E.VideoMessage{
			URL:           proto.String(mediaInfo.URL),
			DirectPath:    proto.String(mediaInfo.DirectPath),
			MediaKey:      mediaInfo.MediaKey,
			FileEncSHA256: mediaInfo.FileEncSHA256,
			FileSHA256:    mediaInfo.FileSHA256,
			FileLength:    proto.Uint64(mediaInfo.FileLength),
			Mimetype:      proto.String(mediaInfo.Mimetype),
		})
	case whatsmeow.MediaAudio:
		data, err = client.Download(context.Background(), &waE2E.AudioMessage{
			URL:           proto.String(mediaInfo.URL),
			DirectPath:    proto.String(mediaInfo.DirectPath),
			MediaKey:      mediaInfo.MediaKey,
			FileEncSHA256: mediaInfo.FileEncSHA256,
			FileSHA256:    mediaInfo.FileSHA256,
			FileLength:    proto.Uint64(mediaInfo.FileLength),
			Mimetype:      proto.String(mediaInfo.Mimetype),
		})
	default: // MediaDocument
		data, err = client.Download(context.Background(), &waE2E.DocumentMessage{
			URL:           proto.String(mediaInfo.URL),
			DirectPath:    proto.String(mediaInfo.DirectPath),
			MediaKey:      mediaInfo.MediaKey,
			FileEncSHA256: mediaInfo.FileEncSHA256,
			FileSHA256:    mediaInfo.FileSHA256,
			FileLength:    proto.Uint64(mediaInfo.FileLength),
			Mimetype:      proto.String(mediaInfo.Mimetype),
		})
	}

	if err != nil {
		log.Error().Err(err).Str("instanceId", instanceID).Msg("Failed to download media")
		return nil, "", fmt.Errorf("failed to download media: %w", err)
	}

	log.Info().
		Str("instanceId", instanceID).
		Int("bytes", len(data)).
		Msg("Media downloaded successfully")

	return data, mediaInfo.Mimetype, nil
}
