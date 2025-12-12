/*
Package go_fcm_receiver provides a simplified FCM (Firebase Cloud Messaging) receiver
for receiving push messages.

Key Features:
- One-time message reception with automatic cleanup
- Proxy support (socks5://username:password@ip:port)
- Thread-safe device registry
- Timeout handling with resource cleanup
- Automatic device shutdown after receiving target message

Architecture:
- Each GetFcmToken() call creates an independent FCMDevice instance
- FCMDevice listens for messages containing "targetMessage" key in AppData
- Messages are delivered through a buffered channel (size=1)
- Device automatically stops after receiving one message or timeout
- Global device registry manages multiple concurrent devices

Thread Safety:
- All device operations are protected by sync.RWMutex
- Safe for concurrent use across multiple goroutines
- Each device has isolated message channel

Configuration:
Update deviceConfig variable with your Firebase/Android app credentials before use.
*/
package go_fcm_receiver

import (
	"fmt"
	"sync"
	"time"
)

type DeviceConfig struct {
	FcmApiKey          string
	FcmAppID           string
	FcmProjectID       string
	GcmSenderID        string
	AndroidPackageCert string
	AndroidPackage     string
	Appver             string
	AppVerName         string
}

type AccountInfo struct {
	FcmToken         string
	GcmToken         string
	AndroidID        uint64
	SecurityToken    uint64
	PrivateKeyBase64 string
	AuthSecretBase64 string
}

// FCMDevice represents a single FCM device instance for one-time push code reception
type FCMDevice struct {
	fcmToken     string
	fcmClient    *FCMClient
	targetMessageChan chan string
}

var deviceConfig DeviceConfig = DeviceConfig{
	FcmApiKey:          "google_api_key",
	FcmAppID:           "google_app_id",
	FcmProjectID:       "project_id",
	GcmSenderID:        "gcm_defaultSenderId",
	AndroidPackageCert: "apk-sha1-fingerprint",
	AndroidPackage:     "package-name",
	Appver:             "apk-version-code",
	AppVerName:         "apk-version-name",
}

// Global device registry to map FCM tokens to device instances
var (
	deviceRegistry = make(map[string]*FCMDevice)
	registryMutex  sync.RWMutex
)

// Register device in global registry
func registerDevice(token string, device *FCMDevice) {
	registryMutex.Lock()
	defer registryMutex.Unlock()
	deviceRegistry[token] = device
}

// Get device from global registry
func getDevice(token string) (*FCMDevice, bool) {
	registryMutex.RLock()
	defer registryMutex.RUnlock()
	device, exists := deviceRegistry[token]
	return device, exists
}

// Unregister device from global registry
func unregisterDevice(token string) {
	registryMutex.Lock()
	defer registryMutex.Unlock()
	if device, exists := deviceRegistry[token]; exists {
		close(device.targetMessageChan)
		delete(deviceRegistry, token)
	}
}

// createOnRawMessageHandler creates a message handler for a specific device
func createOnRawMessageHandler(device *FCMDevice) func(*DataMessageStanza) {
	return func(message *DataMessageStanza) {
		fmt.Printf("[%s] Received raw FCM message for token %s: %+v\n",
			time.Now().Format("2006-01-02 15:04:05"), device.fcmToken, message)

		// Iterate through AppData slice to find targetMessage
		var targetMessage string
		for _, appData := range message.AppData {
			if appData.GetKey() == "targetMessage" {
				targetMessage = appData.GetValue()
				break
			}
		}

		if targetMessage != "" {
			fmt.Printf("Found targetMessage for token %s: %s", device.fcmToken, targetMessage)

			// Send targetMessage to channel (should not block since channel has buffer size 1)
			select {
			case device.targetMessageChan <- targetMessage:
				fmt.Printf("targetMessage sent successfully for token %s", device.fcmToken)
			default:
				// This should rarely happen, but if it does, just log it
				fmt.Printf("targetMessage channel full for token %s, this is unexpected", device.fcmToken)
			}

			// Automatically stop listening after receiving targetMessage
			go func() {
				time.Sleep(100 * time.Millisecond) // Give a moment for the channel consumer to read
				device.fcmClient.Close()
				//unregisterDevice(device.fcmToken)
				fmt.Printf("FCM device auto-stopped after receiving targetMessage for token %s", device.fcmToken)
			}()
		} else {
			fmt.Printf("No targetMessage found in message for token %s", device.fcmToken)
		}
	}
}

func GetMessage(fcmToken string, timeout time.Duration) (string, bool) {
	device, exists := getDevice(fcmToken)
	if !exists {
		fmt.Printf("FCM device not found for token: %s", fcmToken)
		return "", false
	}

	select {
	case targetMessage := <-device.targetMessageChan:
		fmt.Printf("Received targetMessage for token %s: %s", fcmToken, targetMessage)
		return targetMessage, true
	case <-time.After(timeout):
		fmt.Printf("Timeout waiting for targetMessage after %v for token %s", timeout, fcmToken)
		// Clean up on timeout
		device.fcmClient.Close()
		unregisterDevice(fcmToken)
		return "", false
	}
}

// GetFcmToken creates a new FCM device and returns the FCM token
// Each call creates an independent device instance that will automatically
// stop listening after receiving one registration code
// proxyURL: socks5://username:password@ip:port
func GetFcmToken(proxyURL string) string {
	fcmToken, err := createNewDevice(proxyURL)
	if err != nil {
		fmt.Printf("Error creating new device: %v", err)
		return ""
	}

	return fcmToken
}

func createNewDevice(proxyURL string) (string, error) {
	// Create device instance
	device := &FCMDevice{
		targetMessageChan: make(chan string, 1), // Buffer size 1 is enough for one-time reception
	}

	// Create FCM client with device-specific callback
	fcmClient := FCMClient{
		ApiKey:    deviceConfig.FcmApiKey,
		AppId:     deviceConfig.FcmAppID,
		ProjectID: deviceConfig.FcmProjectID,
		OnDataMessage: func(message []byte) {
			fmt.Printf("[%s] Received FCM message for token %s: %s\n",
				time.Now().Format("2006-01-02 15:04:05"), device.fcmToken, string(message))
		},
		OnRawMessage: createOnRawMessageHandler(device),
		AndroidApp: &AndroidFCM{
			GcmSenderId:        deviceConfig.GcmSenderID,
			AndroidPackage:     deviceConfig.AndroidPackage,
			AndroidPackageCert: deviceConfig.AndroidPackageCert,
			AppVer:             deviceConfig.Appver,
			AppVerName:         deviceConfig.AppVerName,
		},
		ProxyURL: proxyURL,
	}

	// Generate new keys
	privateKey, authSecret, err := fcmClient.CreateNewKeys()
	if err != nil {
		fmt.Printf("failed to create new keys: %v", err)
		return "", fmt.Errorf("failed to create new keys: %v", err)
	}

	// Register device
	fcmToken, _, _, _, err := fcmClient.Register()
	if err != nil {
		return "", fmt.Errorf("failed to register device: %v", err)
	}

	// Update device with token and client
	device.fcmToken = fcmToken
	device.fcmClient = &fcmClient

	// Register device in global registry
	registerDevice(fcmToken, device)

	fmt.Printf("Successfully created new device with privateKey length: %d, authSecret length: %d, FCM token: %s",
		len(privateKey), len(authSecret), fcmToken)

	// Start listening in background - simple and direct
	go fcmClient.StartListening()

	return fcmToken, nil
}



func SendPushToken(fcmToken string) error {
	if fcmToken == "" {
		return fmt.Errorf("FCM token is empty")
	}

	// Send push token to server
	// err := sendPushTokenToServer(fcmToken)
	// if err != nil {
	// 	return fmt.Errorf("failed to send push token: %v", err)
	// }

	return nil
}

// Simplified usage example:
func main() {
	// Create FCM device and get token
	fcmToken := GetFcmToken("")
	if fcmToken == "" {
		fmt.Printf("Failed to get FCM token")
	}

	fmt.Printf("FCM Token: %s", fcmToken)
	SendPushToken(fcmToken)

	// Wait for message (will auto-cleanup after receiving)
	message, ok := GetMessage(fcmToken, 30*time.Second)
	if ok {
		fmt.Printf("Received message: %s", message)
		// Device will automatically stop listening and clean up
	} else {
		fmt.Printf("Failed to receive push code within timeout")
	}
}
