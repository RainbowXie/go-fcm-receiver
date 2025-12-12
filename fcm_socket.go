package go_fcm_receiver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/golang/protobuf/proto"
	"sync"
	"time"
)

type FCMSocketHandler struct {
	Socket              *tls.Conn
	HeartbeatInterval   time.Duration
	IsAlive             bool
	state               int
	data                []byte
	dataMutex           sync.Mutex
	sizePacketSoFar     int
	messageTag          int
	messageSize         int
	handshakeComplete   bool
	isWaitingForData    bool
	errChan             chan error
	socketContext       context.Context
	socketContextCancel context.CancelFunc
	onDataMutex         sync.Mutex
	OnMessage           func(messageTag int, messageObject interface{}) error
	// New fields for improved lifecycle management
	contextMutex        sync.RWMutex
	goroutinesWg        sync.WaitGroup
	isClosing           bool
	closingMutex        sync.RWMutex
	initMutex           sync.Mutex  // Protect Init() method from concurrent calls
}

func (f *FCMSocketHandler) StartSocketHandler() error {
	f.contextMutex.Lock()
	f.socketContext, f.socketContextCancel = context.WithCancel(context.Background())
	f.contextMutex.Unlock()

	f.closingMutex.Lock()
	f.isClosing = false
	f.closingMutex.Unlock()

	f.errChan = make(chan error)

	// Start goroutines with proper lifecycle management
	f.goroutinesWg.Add(2)
	go f.readData()
	go f.sendHeartbeatPings()

	return <-f.errChan
}

func (f *FCMSocketHandler) sendHeartbeatPings() {
	defer f.goroutinesWg.Done()

	if f.HeartbeatInterval == 0 {
		f.HeartbeatInterval = time.Minute * 10
	}

	for {
		// Check if we're closing
		f.closingMutex.RLock()
		isClosing := f.isClosing
		f.closingMutex.RUnlock()
		if isClosing {
			return
		}

		// Get context safely
		f.contextMutex.RLock()
		ctx := f.socketContext
		f.contextMutex.RUnlock()

		if ctx == nil {
			return
		}

		select {
		case <-time.After(f.HeartbeatInterval):
			err := f.SendHeartbeatPing()
			if err != nil {
				f.close(err)
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (f *FCMSocketHandler) SendHeartbeatPing() error {
	obj := &HeartbeatPing{}
	data, err := proto.Marshal(obj)
	if err != nil {
		err = errors.New(fmt.Sprintf("failed to marshal a heartbeat ping packet: %s", err.Error()))
		return err
	}
	_, err = f.Socket.Write(append([]byte{KHeartbeatPingTag, byte(proto.Size(obj))}, data...))
	if err != nil {
		err = errors.New(fmt.Sprintf("failed to send a heartbeat ping: %s", err.Error()))
		return err
	}
	return nil
}

func (f *FCMSocketHandler) readData() {
	defer f.goroutinesWg.Done()

	for {
		// Check if we're closing
		f.closingMutex.RLock()
		isClosing := f.isClosing
		f.closingMutex.RUnlock()
		if isClosing {
			return
		}

		// Check if socket is nil before attempting to read
		if f.Socket == nil {
			f.close(errors.New("socket is nil"))
			return
		}

		// Get context safely
		f.contextMutex.RLock()
		ctx := f.socketContext
		f.contextMutex.RUnlock()

		if ctx == nil {
			f.close(errors.New("context is nil"))
			return
		}

		var buffer []byte
		buffer = make([]byte, 1024*32)
		n, err := f.Socket.Read(buffer)
		if err != nil {
			err = errors.New(fmt.Sprintf("failed to read from the FCM socket: %s", err.Error()))
			select {
			case <-ctx.Done():
				return
			default:
				f.close(err)
			}
			return
		}
		f.dataMutex.Lock()
		f.data = append(f.data, buffer[:n]...)
		f.dataMutex.Unlock()
		go f.onData()
	}
}

func (f *FCMSocketHandler) onData() error {
	f.onDataMutex.Lock()
	defer f.onDataMutex.Unlock()

	if f.isWaitingForData {
		f.isWaitingForData = false
		err := f.waitForData()
		if err != nil {
			f.close(err)
			return err
		}
	}
	return nil
}

func (f *FCMSocketHandler) waitForData() error {
	minBytesNeeded := 0

	switch f.state {
	case MCS_VERSION_TAG_AND_SIZE:
		minBytesNeeded = KVersionPacketLen + KTagPacketLen + KSizePacketLenMin
		break
	case MCS_TAG_AND_SIZE:
		minBytesNeeded = KTagPacketLen + KSizePacketLenMin
		break
	case MCS_SIZE:
		minBytesNeeded = f.sizePacketSoFar + 1
		break
	case MCS_PROTO_BYTES:
		minBytesNeeded = f.messageSize
		break
	default:
		err := errors.New(fmt.Sprintf("socket handler is in an unexpected state (%d)", f.state))
		return err
	}

	f.dataMutex.Lock()
	if len(f.data) < minBytesNeeded {
		f.dataMutex.Unlock()
		f.isWaitingForData = true
		return nil
	}
	f.dataMutex.Unlock()

	switch f.state {
	case MCS_VERSION_TAG_AND_SIZE:
		err := f.onGotVersion()
		if err != nil {
			return err
		}
		break
	case MCS_TAG_AND_SIZE:
		err := f.onGotMessageTag()
		if err != nil {
			return err
		}
		break
	case MCS_SIZE:
		err := f.onGotMessageSize()
		if err != nil {
			return err
		}
		break
	case MCS_PROTO_BYTES:
		err := f.onGotMessageBytes()
		if err != nil {
			return err
		}
		break
	default:
		err := errors.New(fmt.Sprintf("socket handler is in an unexpected state (%d)", f.state))
		return err
	}

	return nil
}

func (f *FCMSocketHandler) onGotVersion() error {
	f.dataMutex.Lock()
	if len(f.data) < 1 {
		err := errors.New("version length is invalid")
		return err
	}
	version := int(f.data[0])
	f.data = f.data[1:]
	f.dataMutex.Unlock()

	if version < KMCSVersion && version != 38 {
		err := errors.New(fmt.Sprintf("server returned wrong version (%d)", version))
		return err
	}

	err := f.onGotMessageTag()
	if err != nil {
		return err
	}
	return nil
}

func (f *FCMSocketHandler) onGotMessageTag() error {
	f.dataMutex.Lock()
	f.messageTag = int(f.data[0])
	f.data = f.data[1:]
	f.dataMutex.Unlock()

	err := f.onGotMessageSize()
	if err != nil {
		return err
	}
	return nil
}

func (f *FCMSocketHandler) onGotMessageSize() error {
	incompleteSizePacket := false
	var pos int
	var err error
	f.dataMutex.Lock()
	f.messageSize, pos, err = ReadInt32(f.data)
	f.dataMutex.Unlock()
	pos += 1
	if err != nil {
		incompleteSizePacket = true
	}

	if incompleteSizePacket {
		f.sizePacketSoFar = pos
		f.state = MCS_SIZE
		err = f.waitForData()
		return err
	}

	f.dataMutex.Lock()
	f.data = f.data[pos:]
	f.dataMutex.Unlock()

	f.sizePacketSoFar = 0

	if f.messageSize > 0 {
		f.state = MCS_PROTO_BYTES
		err = f.waitForData()
		if err != nil {
			return err
		}
	} else {
		err = f.onGotMessageBytes()
		if err != nil {
			return err
		}
	}
	return nil
}

func (f *FCMSocketHandler) onGotMessageBytes() error {
	f.dataMutex.Lock()
	if len(f.data) < f.messageSize {
		f.dataMutex.Unlock()
		f.state = MCS_PROTO_BYTES
		err := f.waitForData()
		if err != nil {
			return err
		}
		return nil
	}
	protobuf, err := f.buildProtobufFromTag(f.data[:f.messageSize])
	f.dataMutex.Unlock()
	if err != nil {
		err = errors.New(fmt.Sprintf("failed to re-build protobuf packet from messageTag (%d): %s", f.messageTag, err.Error()))
		return err
	}
	if protobuf == nil {
		f.data = f.data[f.messageSize:]
		err = errors.New(fmt.Sprintf("unknown message tag(%d)", f.messageTag))
		return err
	}

	if f.messageSize == 0 {
		err = f.OnMessage(f.messageTag, nil)
		if err != nil {
			return err
		}
		err = f.getNextMessage()
		if err != nil {
			return err
		}
		return nil
	}

	if len(f.data) < f.messageSize {
		f.state = MCS_PROTO_BYTES
		err = f.waitForData()
		if err != nil {
			return err
		}
		return nil
	}

	f.dataMutex.Lock()
	f.data = f.data[f.messageSize:]
	f.dataMutex.Unlock()

	err = f.OnMessage(f.messageTag, protobuf)
	if err != nil {
		return err
	}

	if f.messageTag == KLoginResponseTag {
		if !f.handshakeComplete {
			f.handshakeComplete = true
		}
	}

	err = f.getNextMessage()
	if err != nil {
		return err
	}
	return nil
}

func (f *FCMSocketHandler) getNextMessage() error {
	f.messageTag = 0
	f.messageSize = 0
	f.state = MCS_TAG_AND_SIZE
	err := f.waitForData()
	if err != nil {
		return err
	}
	return nil
}

func (f *FCMSocketHandler) buildProtobufFromTag(buffer []byte) (interface{}, error) {
	switch f.messageTag {
	case KHeartbeatPingTag:
		heartbeatPing, err := DecodeHeartbeatPing(buffer)
		if err != nil {
			return nil, err
		}
		return heartbeatPing, nil
	case KHeartbeatAckTag:
		heartbeatAck, err := DecodeHeartbeatAck(buffer)
		if err != nil {
			return nil, err
		}
		return heartbeatAck, nil
	case KLoginRequestTag:
		loginRequest, err := DecodeLoginRequest(buffer)
		if err != nil {
			return nil, err
		}
		return loginRequest, nil
	case KLoginResponseTag:
		loginResponse, err := DecodeLoginResponse(buffer)
		if err != nil {
			return nil, err
		}
		return loginResponse, nil
	case KCloseTag:
		closeObject, err := DecodeClose(buffer)
		if err != nil {
			return nil, err
		}
		return closeObject, nil
	case KIqStanzaTag:
		iqStanza, err := DecodeIqStanza(buffer)
		if err != nil {
			return nil, err
		}
		return iqStanza, nil
	case KDataMessageStanzaTag:
		dataMessageStanza, err := DecodeDataMessageStanza(buffer)
		if err != nil {
			return nil, err
		}
		return dataMessageStanza, nil
	case KStreamErrorStanzaTag:
		streamErrorStanza, err := DecodeStreamErrorStanza(buffer)
		if err != nil {
			return nil, err
		}
		return streamErrorStanza, nil
	default:
		return nil, nil
	}
}

func (f *FCMSocketHandler) Init() {
	// Protect from concurrent Init calls
	f.initMutex.Lock()
	defer f.initMutex.Unlock()

	// Wait for any existing goroutines to finish
	f.closingMutex.Lock()
	f.isClosing = true
	f.closingMutex.Unlock()

	// Cancel existing context if it exists
	f.contextMutex.Lock()
	if f.socketContextCancel != nil {
		f.socketContextCancel()
	}
	f.contextMutex.Unlock()

	// Wait for all goroutines to finish
	f.goroutinesWg.Wait()

	// Reset all fields safely
	f.contextMutex.Lock()
	f.socketContext = nil
	f.socketContextCancel = nil
	f.contextMutex.Unlock()

	f.closingMutex.Lock()
	f.isClosing = false
	f.closingMutex.Unlock()

	f.state = MCS_VERSION_TAG_AND_SIZE
	f.dataMutex.Lock()
	f.data = []byte{}
	f.dataMutex.Unlock()
	f.sizePacketSoFar = 0
	f.messageTag = 0
	f.messageSize = 0
	f.handshakeComplete = false
	f.isWaitingForData = true
}

func (f *FCMSocketHandler) close(err error) {
	// Set closing flag to signal all goroutines to stop
	f.closingMutex.Lock()
	if f.isClosing {
		f.closingMutex.Unlock()
		return // Already closing, avoid duplicate close
	}
	f.isClosing = true
	f.closingMutex.Unlock()

	// Cancel context to signal goroutines
	f.contextMutex.Lock()
	if f.socketContextCancel != nil {
		f.socketContextCancel()
	}
	f.contextMutex.Unlock()

	// Close socket
	if f.Socket != nil {
		f.Socket.Close()
	}

	// Mark as not alive
	f.IsAlive = false

	// Send error to channel if possible
	if f.errChan != nil {
		select {
		case f.errChan <- err:
		default:
		}
	}

	// Wait for goroutines to finish
	f.goroutinesWg.Wait()

	// Reset state without calling Init() to avoid recursion
	f.contextMutex.Lock()
	f.socketContext = nil
	f.socketContextCancel = nil
	f.contextMutex.Unlock()

	f.closingMutex.Lock()
	f.isClosing = false
	f.closingMutex.Unlock()
}
