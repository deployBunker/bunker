package main

import (
	"context"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// GAP-072: BunkerdHandler gained the AttachAgent bidirectional RPC. The CLI
// surface tests in this package stub the handler by hand; this keeps the stub
// satisfying the interface with the same CodeUnimplemented contract its other
// unused methods carry.
func (s *rootExecStubServer) AttachAgent(context.Context, *connect.BidiStream[v1.AttachAgentRequest, v1.AttachAgentResponse]) error {
	return connect.NewError(connect.CodeUnimplemented, nil)
}
