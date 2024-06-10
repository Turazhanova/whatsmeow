package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"image/png"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mdp/qrterminal/v3"
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	_ "github.com/mattn/go-sqlite3" // Import the SQLite3 driver
	logrus "github.com/sirupsen/logrus"
)

var (
	client      *whatsmeow.Client
	csvFilePath = "messages.csv"
	csvMutex    sync.Mutex
)

func main() {
	// Setup logging
	logrus.SetLevel(logrus.DebugLevel)

	// Setup database
	container, err := sqlstore.New("sqlite3", "file:whatsmeow.db?_foreign_keys=on", nil)
	if err != nil {
		log.Fatalf("Failed to create container: %v", err)
	}

	deviceStore, err := container.GetFirstDevice()
	if err != nil {
		log.Fatalf("Failed to get device: %v", err)
	}

	// Create client
	client = whatsmeow.NewClient(deviceStore, nil)
	if client.Store.ID == nil {
		qrChannel, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			log.Fatalf("Failed to connect: %v", err)
		}

		go func() {
			for evt := range qrChannel {
				if evt.Event == "code" {
					qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
					saveQRCode(evt.Code)      // Save the QR code to be used by the API
					sendQRCodeToAPI(evt.Code) // Send the QR code to the specified API
				} else {
					log.Printf("QR Channel result: %s", evt.Event)
				}
			}
		}()
	} else {
		err = client.Connect()
		if err != nil {
			log.Fatalf("Failed to connect: %v", err)
		}
	}

	// Handle received messages and other events
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			handleReceivedMessage(v)
		case *events.Connected:
			fmt.Println("Connected to WhatsApp")
		case *events.OfflineSyncCompleted:
			fmt.Println("Offline sync completed")
		case *events.LoggedOut:
			fmt.Println("Logged out")
		case *events.Disconnected:
			fmt.Println("Disconnected")
		default:
			fmt.Printf("Unhandled event: %T\n", v)
		}
	})

	// Set up Gin
	router := gin.Default()
	router.POST("/send", sendMessageHandler)
	router.GET("/qr/text", generateQRTextHandler)   // Add QR code text endpoint
	router.GET("/qr/photo", generateQRPhotoHandler) // Add QR code photo endpoint
	router.GET("/csv", getCSVContentsHandler)       // Add endpoint to get CSV contents
	log.Println("Starting server on port 8080")
	router.Run(":8080")
}

// Function to handle received messages
func handleReceivedMessage(message *events.Message) {
	sender := message.Info.Sender.String()
	msg := message.Message
	timestamp := message.Info.Timestamp.Format(time.RFC3339) // Format the timestamp

	log.Printf("Received message from %s at %s", sender, timestamp)

	if msg.GetConversation() != "" {
		conversation := msg.GetConversation()
		log.Printf("Conversation: %s\n", conversation)
		writeToCSV(sender, "Conversation", conversation, timestamp)

	} else if extendedText := msg.GetExtendedTextMessage(); extendedText != nil {
		extendedTextMsg := extendedText.GetText()
		log.Printf("Extended Text Message: %s\n", extendedTextMsg)
		writeToCSV(sender, "ExtendedText", extendedTextMsg, timestamp)

	} else if imageMessage := msg.GetImageMessage(); imageMessage != nil {
		caption := imageMessage.GetCaption()
		imageData, err := client.Download(imageMessage) // Correctly download image data
		if err != nil {
			log.Printf("Failed to download image data: %v", err)
			return
		}
		log.Println("Received an image message")
		imagePath, err := saveMedia("image", imageData)
		if err != nil {
			log.Printf("Failed to save image: %v", err)
			return
		}
		writeToCSV(sender, "Image", fmt.Sprintf("Caption: %s, Path: %s", caption, imagePath), timestamp)
	} else if videoMessage := msg.GetVideoMessage(); videoMessage != nil {
		caption := videoMessage.GetCaption()
		videoData, err := client.Download(videoMessage) // Correctly download video data
		if err != nil {
			log.Printf("Failed to download video data: %v", err)
			return
		}
		log.Println("Received a video message")
		videoPath, err := saveMedia("video", videoData)
		if err != nil {
			log.Printf("Failed to save video: %v", err)
			return
		}
		writeToCSV(sender, "Video", fmt.Sprintf("Caption: %s, Path: %s", caption, videoPath), timestamp)

	} else if documentMessage := msg.GetDocumentMessage(); documentMessage != nil {
		fileName := documentMessage.GetFileName()
		documentData, err := client.Download(documentMessage) // Correctly download document data
		if err != nil {
			log.Printf("Failed to download document data: %v", err)
			return
		}
		log.Println("Received a document message")
		documentPath, err := saveMedia("document", documentData)
		if err != nil {
			log.Printf("Failed to save document: %v", err)
			return
		}
		writeToCSV(sender, "Document", fmt.Sprintf("FileName: %s, Path: %s", fileName, documentPath), timestamp)

	} else if audioMessage := msg.GetAudioMessage(); audioMessage != nil {
		audioData, err := client.Download(audioMessage) // Correctly download audio data
		if err != nil {
			log.Printf("Failed to download audio data: %v", err)
			return
		}
		log.Println("Received an audio message")
		audioPath, err := saveMedia("audio", audioData)
		if err != nil {
			log.Printf("Failed to save audio: %v", err)
			return
		}
		writeToCSV(sender, "Audio", audioPath, timestamp)

	} else if contactMessage := msg.GetContactMessage(); contactMessage != nil {
		contactName := contactMessage.GetDisplayName()
		log.Println("Received a contact message")
		writeToCSV(sender, "Contact", contactName, timestamp)

	} else if locationMessage := msg.GetLocationMessage(); locationMessage != nil {
		location := fmt.Sprintf("Lat: %f, Long: %f", locationMessage.GetDegreesLatitude(), locationMessage.GetDegreesLongitude())
		log.Println("Received a location message")
		writeToCSV(sender, "Location", location, timestamp)

	} else {
		log.Printf("Received an unhandled message type from %s\n", sender)
		writeToCSV(sender, "Unknown", "Unknown message type", timestamp)
	}
}

// Function to send a text message
func sendTextMessage(client *whatsmeow.Client, jid string, text string) error {
	targetJID := types.NewJID(jid, "s.whatsapp.net")
	msgID := client.GenerateMessageID()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	log.Printf("Sending text message to %s: %s", targetJID.String(), text)
	_, err := client.SendMessage(ctx, targetJID, &waProto.Message{
		Conversation: proto.String(text),
	})
	if err != nil {
		log.Printf("Failed to send text message: %v", err)
		return err
	}
	log.Printf("Text message sent, ID: %s", msgID)

	timestamp := time.Now().Format(time.RFC3339)
	writeToCSV("me", "SentTextMessage", text, timestamp)

	return nil
}

// Function to send an image message
func sendImageMessage(client *whatsmeow.Client, jid string, imagePath string, caption string) error {
	targetJID := types.NewJID(jid, "s.whatsapp.net")
	msgID := client.GenerateMessageID()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Check if the image file exists
	if _, err := os.Stat(imagePath); os.IsNotExist(err) {
		log.Printf("Image file does not exist: %s", imagePath)
		return fmt.Errorf("image file does not exist: %s", imagePath)
	}

	imageData, err := os.ReadFile(imagePath)
	if err != nil {
		log.Printf("Failed to read image file: %v", err)
		return err
	}

	log.Printf("Uploading image file for JID: %s", targetJID.String())
	uploaded, err := client.Upload(ctx, imageData, whatsmeow.MediaImage)
	if err != nil {
		log.Printf("Failed to upload image: %v", err)
		return err
	}

	log.Printf("Sending image message to %s with caption: %s", targetJID.String(), caption)
	_, err = client.SendMessage(ctx, targetJID, &waProto.Message{
		ImageMessage: &waProto.ImageMessage{
			Caption:    proto.String(caption),
			Mimetype:   proto.String("image/jpeg"),
			FileLength: proto.Uint64(uint64(len(imageData))),
			MediaKey:   uploaded.MediaKey,
			DirectPath: proto.String(uploaded.DirectPath),
		},
	})
	if err != nil {
		log.Printf("Failed to send image message: %v", err)
		return err
	}
	log.Printf("Image message sent, ID: %s", msgID)

	timestamp := time.Now().Format(time.RFC3339)
	writeToCSV("me", "SentImageMessage", fmt.Sprintf("Caption: %s, Path: %s", caption, imagePath), timestamp)

	return nil
}

// Function to send a video message
func sendVideoMessage(client *whatsmeow.Client, jid string, videoPath string, caption string) error {
	targetJID := types.NewJID(jid, "s.whatsapp.net")
	msgID := client.GenerateMessageID()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	videoData, err := os.ReadFile(videoPath)
	if err != nil {
		log.Printf("Failed to read video file: %v", err)
		return err
	}

	log.Printf("Uploading video file for JID: %s", targetJID.String())
	uploaded, err := client.Upload(ctx, videoData, whatsmeow.MediaVideo)
	if err != nil {
		log.Printf("Failed to upload video: %v", err)
		return err
	}

	log.Printf("Sending video message to %s with caption: %s", targetJID.String(), caption)
	_, err = client.SendMessage(ctx, targetJID, &waProto.Message{
		VideoMessage: &waProto.VideoMessage{
			Caption:    proto.String(caption),
			Mimetype:   proto.String("video/mp4"),
			FileLength: proto.Uint64(uint64(len(videoData))),
			MediaKey:   uploaded.MediaKey,
			DirectPath: proto.String(uploaded.DirectPath),
		},
	})
	if err != nil {
		log.Printf("Failed to send video message: %v", err)
		return err
	}
	log.Printf("Video message sent, ID: %s", msgID)

	timestamp := time.Now().Format(time.RFC3339)
	writeToCSV("me", "SentVideoMessage", fmt.Sprintf("Caption: %s, Path: %s", caption, videoPath), timestamp)

	return nil
}

// Handler to send a message
func sendMessageHandler(c *gin.Context) {
	// Check if the request is a multipart form data
	if c.ContentType() == "multipart/form-data" {
		handleMultipartMessage(c)
	} else {
		handleJSONMessage(c)
	}
}

func handleJSONMessage(c *gin.Context) {
	var request struct {
		JID     string `json:"Jid" binding:"required"`
		Type    string `json:"Type" binding:"required"`
		Content string `json:"Content" binding:"required"`
		Caption string `json:"Caption"` // Optional, only used for image and video messages
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		log.Println("Failed to bind JSON:", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	log.Println("Received JSON request to send message:", request)
	var err error
	switch request.Type {
	case "text":
		err = sendTextMessage(client, request.JID, request.Content)
	case "image":
		err = sendImageMessage(client, request.JID, request.Content, request.Caption)
	case "video":
		err = sendVideoMessage(client, request.JID, request.Content, request.Caption)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid message type for JSON"})
		return
	}

	if err != nil {
		log.Println("Failed to send message:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	log.Println("Message sent successfully")
	c.JSON(http.StatusOK, gin.H{"status": "Message sent"})
}

func handleMultipartMessage(c *gin.Context) {
	jid := c.PostForm("Jid")
	messageType := c.PostForm("Type")
	caption := c.PostForm("Caption")

	log.Printf("Received multipart request to send message: JID=%s, Type=%s, Caption=%s", jid, messageType, caption)

	var err error
	switch messageType {
	case "image":
		file, err := c.FormFile("Content")
		if err != nil {
			log.Println("Failed to get file from form:", err)
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		log.Printf("Received file for image: %s", file.Filename)
		// Save the file
		filePath := fmt.Sprintf("media/image/%s", file.Filename)
		if err := c.SaveUploadedFile(file, filePath); err != nil {
			log.Println("Failed to save file:", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		err = sendImageMessage(client, jid, filePath, caption)
	case "video":
		file, err := c.FormFile("Content")
		if err != nil {
			log.Println("Failed to get file from form:", err)
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		log.Printf("Received file for video: %s", file.Filename)
		// Save the file
		filePath := fmt.Sprintf("media/video/%s", file.Filename)
		if err := c.SaveUploadedFile(file, filePath); err != nil {
			log.Println("Failed to save file:", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		err = sendVideoMessage(client, jid, filePath, caption)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid message type for multipart"})
		return
	}

	if err != nil {
		log.Println("Failed to send message:", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	log.Println("Message sent successfully")
	c.JSON(http.StatusOK, gin.H{"status": "Message sent"})
}

// Function to save QR code to a file
func saveQRCode(code string) {
	file, err := os.Create("qrcode.txt")
	if err != nil {
		log.Printf("Failed to create QR code file: %v", err)
		return
	}
	defer file.Close()

	_, err = file.WriteString(code)
	if err != nil {
		log.Printf("Failed to write QR code to file: %v", err)
	}
}

// Function to send QR code to the specified API
func sendQRCodeToAPI(code string) {
	jsonData := map[string]string{"qr_code": code}
	jsonValue, err := json.Marshal(jsonData)
	if err != nil {
		log.Printf("Failed to marshal JSON: %v", err)
		return
	}

	resp, err := http.Post("https://devapi.courstore.com/v1/qr/for_login", "application/json", bytes.NewBuffer(jsonValue))
	if err != nil {
		log.Printf("Failed to send POST request: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Received non-OK response from API: %v", resp.Status)
	} else {
		log.Println("QR code sent successfully to the API")
	}
}

// Handler to generate and send QR code as text
func generateQRTextHandler(c *gin.Context) {
	code, err := os.ReadFile("qrcode.txt")
	if err != nil {
		log.Printf("Failed to read QR code file: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate QR code"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"qr_code": string(code)})
}

// Handler to generate and send QR code as photo
func generateQRPhotoHandler(c *gin.Context) {
	code, err := os.ReadFile("qrcode.txt")
	if err != nil {
		log.Printf("Failed to read QR code file: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate QR code"})
		return
	}

	qr, err := qrcode.New(string(code), qrcode.Medium)
	if err != nil {
		log.Printf("Failed to generate QR code image: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate QR code"})
		return
	}

	var pngBuffer bytes.Buffer
	err = png.Encode(&pngBuffer, qr.Image(256))
	if err != nil {
		log.Printf("Failed to encode QR code image: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate QR code"})
		return
	}

	c.Header("Content-Type", "image/png")
	c.Writer.Write(pngBuffer.Bytes())
}

// Function to write a message to the CSV file
func writeToCSV(sender string, messageType string, message string, timestamp string) {
	csvMutex.Lock()
	defer csvMutex.Unlock()

	// Create file if it doesn't exist and open it in append mode
	file, err := os.OpenFile(csvFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Failed to open CSV file: %v", err)
		return
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	// Check if the file is empty to write headers
	info, err := file.Stat()
	if err != nil {
		log.Printf("Failed to get file info: %v", err)
		return
	}

	if info.Size() == 0 {
		// Write headers
		err = writer.Write([]string{"id", "phone", "type", "text", "datetime"})
		if err != nil {
			log.Printf("Failed to write headers to CSV file: %v", err)
			return
		}
	}

	// Write message to CSV
	err = writer.Write([]string{fmt.Sprintf("%d", time.Now().UnixNano()), sender, messageType, message, timestamp})
	if err != nil {
		log.Printf("Failed to write to CSV file: %v", err)
	}
}

// Handler to get CSV contents
func getCSVContentsHandler(c *gin.Context) {
	file, err := os.Open(csvFilePath)
	if err != nil {
		log.Printf("Failed to open CSV file: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to open CSV file"})
		return
	}
	defer file.Close()

	reader := csv.NewReader(file)
	records, err := reader.ReadAll()
	if err != nil {
		log.Printf("Failed to read CSV file: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read CSV file"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": records})
}

// Function to save media files
func saveMedia(mediaType string, mediaData []byte) (string, error) {
	// Define the directory and filename based on the media type and current timestamp
	dir := fmt.Sprintf("media/%s", mediaType)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("Failed to create directory: %v", err)
		return "", err
	}
	var extension string
	switch mediaType {
	case "image":
		extension = ".jpg" // or ".png", depending on your use case
	case "video":
		extension = ".mp4"
	case "audio":
		extension = ".ogg" // or ".mp3", depending on your use case
	case "document":
		extension = ".pdf" // or any other relevant extension
	default:
		extension = ""
	}

	filename := fmt.Sprintf("%s/%d%s", dir, time.Now().UnixNano(), extension)

	// Write the media data to the file
	err := os.WriteFile(filename, mediaData, 0644)
	if err != nil {
		log.Printf("Failed to save media file: %v", err)
		return "", err
	}

	log.Printf("Saved media file: %s", filename) // Log the path of the saved file
	return filename, nil
}
