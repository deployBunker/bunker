package main

import (
	"context"

	"connectrpc.com/connect"

	v1 "github.com/deployBunker/bunker/proto/bunker/v1"
)

// GAP-066 added three RPCs to the Bunkerd service; the root-command test stub
// must satisfy the generated handler interface. They are functional no-ops here
// because no root-command test exercises the alias surface.

func (s *rootExecStubServer) ListProgramAliases(context.Context, *connect.Request[v1.ListProgramAliasesRequest]) (*connect.Response[v1.ListProgramAliasesResponse], error) {
	return connect.NewResponse(&v1.ListProgramAliasesResponse{}), nil
}

func (s *rootExecStubServer) PutProgramAlias(_ context.Context, req *connect.Request[v1.PutProgramAliasRequest]) (*connect.Response[v1.PutProgramAliasResponse], error) {
	return connect.NewResponse(&v1.PutProgramAliasResponse{Alias: req.Msg.GetAlias(), Status: "created"}), nil
}

func (s *rootExecStubServer) DeleteProgramAlias(_ context.Context, req *connect.Request[v1.DeleteProgramAliasRequest]) (*connect.Response[v1.DeleteProgramAliasResponse], error) {
	return connect.NewResponse(&v1.DeleteProgramAliasResponse{Name: req.Msg.GetName(), Deleted: true}), nil
}
