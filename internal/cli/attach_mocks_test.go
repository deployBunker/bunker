package cli

import (
	"context"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// GAP-072: BunkerdHandler gained AttachAgent, and every CLI test mock
// implements the handler interface by hand. Rather than grow a dozen test
// files with the same four lines, the new method for every mock lives here,
// one place, with exactly the contract the other unimplemented methods carry
// (CodeUnimplemented). A mock that needs real attach behavior supplies its own
// method and shadows this one.
func attachUnimplemented() error { return connect.NewError(connect.CodeUnimplemented, nil) }

func (m *metricsMockServer) AttachAgent(context.Context, *connect.BidiStream[v1.AttachAgentRequest, v1.AttachAgentResponse]) error {
	return attachUnimplemented()
}

func (m *heartbeatMockServer) AttachAgent(context.Context, *connect.BidiStream[v1.AttachAgentRequest, v1.AttachAgentResponse]) error {
	return attachUnimplemented()
}

func (m *mockExecServer) AttachAgent(context.Context, *connect.BidiStream[v1.AttachAgentRequest, v1.AttachAgentResponse]) error {
	return attachUnimplemented()
}

func (m *mockRunServer) AttachAgent(context.Context, *connect.BidiStream[v1.AttachAgentRequest, v1.AttachAgentResponse]) error {
	return attachUnimplemented()
}

func (m *mockEnvServer) AttachAgent(context.Context, *connect.BidiStream[v1.AttachAgentRequest, v1.AttachAgentResponse]) error {
	return attachUnimplemented()
}

func (m *cpMockServer) AttachAgent(context.Context, *connect.BidiStream[v1.AttachAgentRequest, v1.AttachAgentResponse]) error {
	return attachUnimplemented()
}

func (m *deployMockServer) AttachAgent(context.Context, *connect.BidiStream[v1.AttachAgentRequest, v1.AttachAgentResponse]) error {
	return attachUnimplemented()
}

func (m *mockBunkerdServer) AttachAgent(context.Context, *connect.BidiStream[v1.AttachAgentRequest, v1.AttachAgentResponse]) error {
	return attachUnimplemented()
}

func (m *mockRunFlagServer) AttachAgent(context.Context, *connect.BidiStream[v1.AttachAgentRequest, v1.AttachAgentResponse]) error {
	return attachUnimplemented()
}

func (m *infoMockServer) AttachAgent(context.Context, *connect.BidiStream[v1.AttachAgentRequest, v1.AttachAgentResponse]) error {
	return attachUnimplemented()
}

func (m *statusMockServer) AttachAgent(context.Context, *connect.BidiStream[v1.AttachAgentRequest, v1.AttachAgentResponse]) error {
	return attachUnimplemented()
}
