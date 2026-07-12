package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestShutdownClosesHTTPThenAppThenDatabaseAndJoinsErrors(t *testing.T) {
	serverErr := errors.New("server close failed")
	appErr := errors.New("app close failed")
	var order []string
	err := shutdown(
		context.Background(),
		func(context.Context) error { order = append(order, "http"); return serverErr },
		func(context.Context) error { order = append(order, "app"); return appErr },
		func() error { order = append(order, "db"); return nil },
	)
	if !reflect.DeepEqual(order, []string{"http", "app", "db"}) {
		t.Fatalf("shutdown order=%v", order)
	}
	if !errors.Is(err, serverErr) || !errors.Is(err, appErr) {
		t.Fatalf("shutdown error=%v", err)
	}
}
