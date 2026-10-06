package cli

import (
	"context"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// GAP-066 added three RPCs to the Bunkerd service, and every CLI test mock
// must satisfy the generated handler interface. They are defined once here for
// every mock type in this package rather than repeated per mock file: the
// alias surface has no CLI-test-specific behaviour, so a shared, functional
// stub is both smaller and less likely to rot than nine copies.

func aliasMockListProgramAliases() (*connect.Response[v1.ListProgramAliasesResponse], error) {
	return connect.NewResponse(&v1.ListProgramAliasesResponse{}), nil
}

func aliasMockPutProgramAlias(req *connect.Request[v1.PutProgramAliasRequest]) (*connect.Response[v1.PutProgramAliasResponse], error) {
	return connect.NewResponse(&v1.PutProgramAliasResponse{
		Alias:  req.Msg.GetAlias(),
		Status: "created",
	}), nil
}

func aliasMockDeleteProgramAlias(req *connect.Request[v1.DeleteProgramAliasRequest]) (*connect.Response[v1.DeleteProgramAliasResponse], error) {
	return connect.NewResponse(&v1.DeleteProgramAliasResponse{
		Name:    req.Msg.GetName(),
		Deleted: true,
	}), nil
}

func (m *metricsMockServer) ListProgramAliases(context.Context, *connect.Request[v1.ListProgramAliasesRequest]) (*connect.Response[v1.ListProgramAliasesResponse], error) {
	return aliasMockListProgramAliases()
}

func (m *metricsMockServer) PutProgramAlias(_ context.Context, req *connect.Request[v1.PutProgramAliasRequest]) (*connect.Response[v1.PutProgramAliasResponse], error) {
	return aliasMockPutProgramAlias(req)
}

func (m *metricsMockServer) DeleteProgramAlias(_ context.Context, req *connect.Request[v1.DeleteProgramAliasRequest]) (*connect.Response[v1.DeleteProgramAliasResponse], error) {
	return aliasMockDeleteProgramAlias(req)
}

func (m *heartbeatMockServer) ListProgramAliases(context.Context, *connect.Request[v1.ListProgramAliasesRequest]) (*connect.Response[v1.ListProgramAliasesResponse], error) {
	return aliasMockListProgramAliases()
}

func (m *heartbeatMockServer) PutProgramAlias(_ context.Context, req *connect.Request[v1.PutProgramAliasRequest]) (*connect.Response[v1.PutProgramAliasResponse], error) {
	return aliasMockPutProgramAlias(req)
}

func (m *heartbeatMockServer) DeleteProgramAlias(_ context.Context, req *connect.Request[v1.DeleteProgramAliasRequest]) (*connect.Response[v1.DeleteProgramAliasResponse], error) {
	return aliasMockDeleteProgramAlias(req)
}

func (m *cpMockServer) ListProgramAliases(context.Context, *connect.Request[v1.ListProgramAliasesRequest]) (*connect.Response[v1.ListProgramAliasesResponse], error) {
	return aliasMockListProgramAliases()
}

func (m *cpMockServer) PutProgramAlias(_ context.Context, req *connect.Request[v1.PutProgramAliasRequest]) (*connect.Response[v1.PutProgramAliasResponse], error) {
	return aliasMockPutProgramAlias(req)
}

func (m *cpMockServer) DeleteProgramAlias(_ context.Context, req *connect.Request[v1.DeleteProgramAliasRequest]) (*connect.Response[v1.DeleteProgramAliasResponse], error) {
	return aliasMockDeleteProgramAlias(req)
}

func (m *keysMockServer) ListProgramAliases(context.Context, *connect.Request[v1.ListProgramAliasesRequest]) (*connect.Response[v1.ListProgramAliasesResponse], error) {
	return aliasMockListProgramAliases()
}

func (m *keysMockServer) PutProgramAlias(_ context.Context, req *connect.Request[v1.PutProgramAliasRequest]) (*connect.Response[v1.PutProgramAliasResponse], error) {
	return aliasMockPutProgramAlias(req)
}

func (m *keysMockServer) DeleteProgramAlias(_ context.Context, req *connect.Request[v1.DeleteProgramAliasRequest]) (*connect.Response[v1.DeleteProgramAliasResponse], error) {
	return aliasMockDeleteProgramAlias(req)
}

func (m *deployMockServer) ListProgramAliases(context.Context, *connect.Request[v1.ListProgramAliasesRequest]) (*connect.Response[v1.ListProgramAliasesResponse], error) {
	return aliasMockListProgramAliases()
}

func (m *deployMockServer) PutProgramAlias(_ context.Context, req *connect.Request[v1.PutProgramAliasRequest]) (*connect.Response[v1.PutProgramAliasResponse], error) {
	return aliasMockPutProgramAlias(req)
}

func (m *deployMockServer) DeleteProgramAlias(_ context.Context, req *connect.Request[v1.DeleteProgramAliasRequest]) (*connect.Response[v1.DeleteProgramAliasResponse], error) {
	return aliasMockDeleteProgramAlias(req)
}

func (m *mockBunkerdServer) ListProgramAliases(context.Context, *connect.Request[v1.ListProgramAliasesRequest]) (*connect.Response[v1.ListProgramAliasesResponse], error) {
	return aliasMockListProgramAliases()
}

func (m *mockBunkerdServer) PutProgramAlias(_ context.Context, req *connect.Request[v1.PutProgramAliasRequest]) (*connect.Response[v1.PutProgramAliasResponse], error) {
	return aliasMockPutProgramAlias(req)
}

func (m *mockBunkerdServer) DeleteProgramAlias(_ context.Context, req *connect.Request[v1.DeleteProgramAliasRequest]) (*connect.Response[v1.DeleteProgramAliasResponse], error) {
	return aliasMockDeleteProgramAlias(req)
}

func (m *infoMockServer) ListProgramAliases(context.Context, *connect.Request[v1.ListProgramAliasesRequest]) (*connect.Response[v1.ListProgramAliasesResponse], error) {
	return aliasMockListProgramAliases()
}

func (m *infoMockServer) PutProgramAlias(_ context.Context, req *connect.Request[v1.PutProgramAliasRequest]) (*connect.Response[v1.PutProgramAliasResponse], error) {
	return aliasMockPutProgramAlias(req)
}

func (m *infoMockServer) DeleteProgramAlias(_ context.Context, req *connect.Request[v1.DeleteProgramAliasRequest]) (*connect.Response[v1.DeleteProgramAliasResponse], error) {
	return aliasMockDeleteProgramAlias(req)
}

func (m *statusMockServer) ListProgramAliases(context.Context, *connect.Request[v1.ListProgramAliasesRequest]) (*connect.Response[v1.ListProgramAliasesResponse], error) {
	return aliasMockListProgramAliases()
}

func (m *statusMockServer) PutProgramAlias(_ context.Context, req *connect.Request[v1.PutProgramAliasRequest]) (*connect.Response[v1.PutProgramAliasResponse], error) {
	return aliasMockPutProgramAlias(req)
}

func (m *statusMockServer) DeleteProgramAlias(_ context.Context, req *connect.Request[v1.DeleteProgramAliasRequest]) (*connect.Response[v1.DeleteProgramAliasResponse], error) {
	return aliasMockDeleteProgramAlias(req)
}
