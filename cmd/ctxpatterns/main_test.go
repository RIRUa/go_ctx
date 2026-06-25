package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rirua/go_ctx/internal/esfake"
)

// TestPattern1_ChildTimeoutDoesNotKillParent は、ES 個別予算が切れても親(サーバー)が
// 生存することを検証します（パターン1）。
func TestPattern1_ChildTimeoutDoesNotKillParent(t *testing.T) {
	serverCtx, cancelServer := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelServer()

	client := esfake.New(30*time.Millisecond, 300*time.Millisecond) // 個別30ms / 応答300ms
	_, err := client.Search(serverCtx, "q")
	if !errors.Is(err, esfake.ErrBudgetExceeded) {
		t.Fatalf("ES 個別予算超過を期待したが: %v", err)
	}
	if serverCtx.Err() != nil {
		t.Fatalf("親(サーバー)は生きているはず: %v", serverCtx.Err())
	}
}

// TestPattern1_ParentTimeoutPropagatesToChild は、親(サーバー)が切れると派生した子も
// 止まることを検証します（親→子は伝播）。
func TestPattern1_ParentTimeoutPropagatesToChild(t *testing.T) {
	serverCtx, cancelServer := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelServer()

	client := esfake.New(500*time.Millisecond, 300*time.Millisecond) // 個別は余裕
	_, err := client.Search(serverCtx, "q")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("親由来の DeadlineExceeded を期待したが: %v", err)
	}
	if errors.Is(err, esfake.ErrBudgetExceeded) {
		t.Fatalf("ES 個別予算ではなく親の deadline が原因のはず: %v", err)
	}
}

// TestPattern2_ServerTimeoutReachesChild は、独立に作った ES context へサーバーの
// timeout が一方向で伝播することを検証します（パターン2）。
func TestPattern2_ServerTimeoutReachesChild(t *testing.T) {
	serverCtx, cancelServer := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelServer()

	client := esfake.New(500*time.Millisecond, 300*time.Millisecond) // 個別は余裕
	_, err := client.SearchWithServerPropagation(serverCtx, "q")
	// 伝播 cause はサーバーの deadline を %w で包んでいるので errors.Is が通る。
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("サーバー由来の DeadlineExceeded を期待したが: %v", err)
	}
}

// TestForcedCancel_PropagatesAsCanceled は、deadline ではなく明示的な cancel() が
// 両パターンとも子へ伝播し、原因が context.Canceled になる（timeout と区別できる）
// ことを検証します。
func TestForcedCancel_PropagatesAsCanceled(t *testing.T) {
	check := func(name string, search func(context.Context, *esfake.Client) error) {
		serverCtx, cancelServer := context.WithCancel(context.Background())
		defer cancelServer()
		go func() { time.Sleep(20 * time.Millisecond); cancelServer() }()

		// 予算は十分(個別500ms / 応答300ms)。強制 cancel が先に効く。
		client := esfake.New(500*time.Millisecond, 300*time.Millisecond)
		err := search(serverCtx, client)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%s: Canceled を期待したが: %v", name, err)
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, esfake.ErrBudgetExceeded) {
			t.Fatalf("%s: timeout 系と誤認されている: %v", name, err)
		}
	}

	check("パターン1", func(ctx context.Context, c *esfake.Client) error {
		_, err := c.Search(ctx, "q")
		return err
	})
	check("パターン2", func(ctx context.Context, c *esfake.Client) error {
		_, err := c.SearchWithServerPropagation(ctx, "q")
		return err
	})
}

// TestPattern2_ChildTimeoutDoesNotReachServer は、ES 個別予算が切れても
// サーバー context は無傷であることを検証します（子→親は非伝播）。
func TestPattern2_ChildTimeoutDoesNotReachServer(t *testing.T) {
	serverCtx, cancelServer := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelServer()

	client := esfake.New(30*time.Millisecond, 300*time.Millisecond) // 個別30ms
	_, err := client.SearchWithServerPropagation(serverCtx, "q")
	if !errors.Is(err, esfake.ErrBudgetExceeded) {
		t.Fatalf("ES 個別予算超過を期待したが: %v", err)
	}
	if serverCtx.Err() != nil {
		t.Fatalf("サーバーは無傷のはず: %v", serverCtx.Err())
	}
}
