package go_fcm_receiver

import (
	"testing"
)

func TestFCMSocketHandler_CloseWithoutInit(t *testing.T) {
	// Test that calling close() on an uninitialized FCMSocketHandler doesn't panic
	handler := &FCMSocketHandler{}
	
	// This should not panic even though socketContextCancel is nil
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("close() panicked when socketContextCancel was nil: %v", r)
		}
	}()
	
	// Call close with a test error - this previously would panic
	handler.close(nil)
}

func TestFCMSocketHandler_CloseAfterInit(t *testing.T) {
	// Test that calling close() after Init() works properly
	handler := &FCMSocketHandler{}
	handler.Init()
	
	// This should not panic
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("close() panicked after Init(): %v", r)
		}
	}()
	
	handler.close(nil)
}

func TestFCMSocketHandler_CloseAfterStartSocketHandler(t *testing.T) {
	// Test that calling close() after StartSocketHandler initialization works
	handler := &FCMSocketHandler{}
	
	// Simulate the context initialization that happens in StartSocketHandler
	// without actually starting the socket handler
	handler.socketContext, handler.socketContextCancel = nil, func() {}
	
	// This should not panic
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("close() panicked after context initialization: %v", r)
		}
	}()
	
	handler.close(nil)
}

func TestFCMSocketHandler_InitResetsContextFields(t *testing.T) {
	// Test that Init() properly resets context fields to nil
	handler := &FCMSocketHandler{}
	
	// Set some non-nil values
	handler.socketContextCancel = func() {}
	
	// Call Init
	handler.Init()
	
	// Verify fields are reset to nil
	if handler.socketContext != nil {
		t.Error("Init() should reset socketContext to nil")
	}
	if handler.socketContextCancel != nil {
		t.Error("Init() should reset socketContextCancel to nil")
	}
}