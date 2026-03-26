package main

import (
	"context"

	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/companions/lsp"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	"github.com/codefly-dev/core/languages"

	"github.com/codefly-dev/core/wool"
)

// Code implements the CodeInterface for the go-grpc service agent.
// It delegates symbol queries to the language-agnostic LSP client.
type Code struct {
	services.CodeServer

	*Service

	// LSP client (lazy-initialized on first call)
	lspClient lsp.Client
}

func NewCode() *Code {
	return &Code{
		Service: NewService(),
	}
}

func (c *Code) ListSymbols(ctx context.Context, req *codev0.ListSymbolsRequest) (*codev0.ListSymbolsResponse, error) {
	defer c.Wool.Catch()
	ctx = c.Wool.Inject(ctx)

	w := wool.Get(ctx).In("Code.ListSymbols")

	// Lazy-initialize the LSP client
	if c.lspClient == nil {
		sourceDir := c.sourceLocation
		if sourceDir == "" {
			// Fall back to base location + configured source dir
			sourceDir = c.Location + "/" + c.Settings.GoSourceDir()
		}

		w.Info("starting LSP client", wool.DirField(sourceDir))

		client, err := lsp.NewClient(ctx, languages.GO, sourceDir)
		if err != nil {
			return &codev0.ListSymbolsResponse{
				Status: &codev0.ListSymbolsStatus{
					State:   codev0.ListSymbolsStatus_ERROR,
					Message: err.Error(),
				},
			}, nil
		}
		c.lspClient = client
	}

	symbols, err := c.lspClient.ListSymbols(ctx, req.File)
	if err != nil {
		return &codev0.ListSymbolsResponse{
			Status: &codev0.ListSymbolsStatus{
				State:   codev0.ListSymbolsStatus_ERROR,
				Message: err.Error(),
			},
		}, nil
	}

	w.Info("found symbols", wool.Field("count", len(symbols)))

	return &codev0.ListSymbolsResponse{
		Status: &codev0.ListSymbolsStatus{
			State: codev0.ListSymbolsStatus_SUCCESS,
		},
		Symbols: symbols,
	}, nil
}
