package service

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/collection"
)

func TestProviderSetRegistersVideoPricingService(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	require.True(t, ok)

	wireFile, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(testFile), "wire.go"), nil, 0)
	require.NoError(t, err)

	registered := false
	ast.Inspect(wireFile, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "NewSet" {
			return true
		}
		packageName, ok := selector.X.(*ast.Ident)
		if !ok || packageName.Name != "wire" {
			return true
		}
		for _, argument := range call.Args {
			provider, ok := argument.(*ast.Ident)
			if ok && provider.Name == "NewVideoPricingService" {
				registered = true
			}
		}
		return false
	})

	require.True(t, registered, "service.ProviderSet must provide VideoPricingService to future Wire consumers")
}

func TestProvideTimingWheelService_ReturnsError(t *testing.T) {
	original := newTimingWheel
	t.Cleanup(func() { newTimingWheel = original })

	newTimingWheel = func(_ time.Duration, _ int, _ collection.Execute) (*collection.TimingWheel, error) {
		return nil, errors.New("boom")
	}

	svc, err := ProvideTimingWheelService()
	if err == nil {
		t.Fatalf("期望返回 error，但得到 nil")
	}
	if svc != nil {
		t.Fatalf("期望返回 nil svc，但得到非空")
	}
}

func TestProvideTimingWheelService_Success(t *testing.T) {
	svc, err := ProvideTimingWheelService()
	if err != nil {
		t.Fatalf("期望 err 为 nil，但得到: %v", err)
	}
	if svc == nil {
		t.Fatalf("期望 svc 非空，但得到 nil")
	}
	svc.Stop()
}
