package main

import (
	"context"
	"log"
	"net/http"
	"time"
)

func stopDOPBatchAdmission(handler http.Handler) {
	if manager, ok := handler.(interface{ StopAccepting() }); ok {
		manager.StopAccepting()
	}
}

func shutdownDOPBatchHandler(handler http.Handler) {
	if manager, ok := handler.(interface{ Shutdown(context.Context) error }); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Shutdown(ctx); err != nil {
			log.Printf("DOP batch shutdown did not drain within the cleanup deadline")
		}
	}
}
