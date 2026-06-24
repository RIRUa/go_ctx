package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestPropagation_ServerCancelReachesChild は、サーバー context が切れると
// 独立に作った esCtx へ一方向で伝播することを検証します。
func TestPropagation_ServerCancelReachesChild(t *testing.T) {
	serverCtx, cancelServer := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelServer()

	esCtx, cancelES, stop := newESContextWithServerPropagation(serverCtx, 500*time.Millisecond)
	defer stop()
	defer cancelES()

	select {
	case <-esCtx.Done():
		// サーバー由来の cause が伝わっているはず。
		if !errors.Is(context.Cause(esCtx), context.DeadlineExceeded) {
			t.Fatalf("サーバー由来の DeadlineExceeded を期待したが: %v", context.Cause(esCtx))
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("サーバー timeout が esCtx に伝播しなかった")
	}
}

// TestPropagation_ChildDoesNotReachServer は、ES 個別予算が切れても
// サーバー context は影響を受けない（子→親は伝播しない）ことを検証します。
func TestPropagation_ChildDoesNotReachServer(t *testing.T) {
	serverCtx, cancelServer := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelServer()

	esCtx, cancelES, stop := newESContextWithServerPropagation(serverCtx, 30*time.Millisecond)
	defer stop()
	defer cancelES()

	<-esCtx.Done()
	if !errors.Is(context.Cause(esCtx), errESBudget) {
		t.Fatalf("ES 個別予算超過を期待したが: %v", context.Cause(esCtx))
	}
	if serverCtx.Err() != nil {
		t.Fatalf("サーバーは無傷のはず: %v", serverCtx.Err())
	}
}
