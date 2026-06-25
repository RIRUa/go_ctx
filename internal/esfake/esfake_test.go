package esfake_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rirua/go_ctx/internal/esfake"
)

// TestDerivedChildShorterWins は、親(サーバー)から派生させた子(ES)の timeout が
// 短いとき、子の deadline で打ち切られ、親は生き残ることを検証します。
func TestDerivedChildShorterWins(t *testing.T) {
	serverCtx, cancelServer := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelServer()
	esCtx, cancelES := context.WithTimeout(serverCtx, 50*time.Millisecond)
	defer cancelES()

	_, err := esfake.Search(esCtx, "q", 300*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("子の deadline 超過を期待したが: %v", err)
	}
	if serverCtx.Err() != nil {
		t.Fatalf("親(サーバー)は生きているはず: %v", serverCtx.Err())
	}
}

// TestDerivedParentShorterWins は、親(サーバー)の deadline が短いとき、子(ES)が
// いくら長い timeout を張っても親が勝つことを検証します。
func TestDerivedParentShorterWins(t *testing.T) {
	serverCtx, cancelServer := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelServer()
	esCtx, cancelES := context.WithTimeout(serverCtx, 500*time.Millisecond)
	defer cancelES()

	// 派生 context の deadline は親側に丸められる。
	dl, _ := esCtx.Deadline()
	if time.Until(dl) > 100*time.Millisecond {
		t.Fatalf("子の deadline が親に丸められていない: 残り %v", time.Until(dl))
	}

	_, err := esfake.Search(esCtx, "q", 300*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("親由来の deadline 超過を期待したが: %v", err)
	}
}

// TestIndependentContextsDoNotPropagate は、独立に作った 2 つの context では
// 一方が切れても他方に伝播しないことを検証します。
func TestIndependentContextsDoNotPropagate(t *testing.T) {
	serverCtx, cancelServer := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelServer()
	esCtx, cancelES := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelES()

	// esCtx だけを渡すと serverCtx が切れても止まらず完走する。
	res, err := esfake.Search(esCtx, "q", 200*time.Millisecond)
	if err != nil {
		t.Fatalf("独立 context なので成功するはず: %v", err)
	}
	if res == "" {
		t.Fatal("結果が空")
	}
	if serverCtx.Err() == nil {
		t.Fatal("serverCtx はこの時点で切れているはず")
	}
}
