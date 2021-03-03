/*
Copyright 2020 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vtadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"

	"vitess.io/vitess/go/trace"
	"vitess.io/vitess/go/vt/concurrency"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/vtadmin/cluster"
	"vitess.io/vitess/go/vt/vtadmin/errors"
	"vitess.io/vitess/go/vt/vtadmin/grpcserver"
	vtadminhttp "vitess.io/vitess/go/vt/vtadmin/http"
	vthandlers "vitess.io/vitess/go/vt/vtadmin/http/handlers"
	"vitess.io/vitess/go/vt/vtadmin/sort"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtexplain"

	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	vtadminpb "vitess.io/vitess/go/vt/proto/vtadmin"
	vtctldatapb "vitess.io/vitess/go/vt/proto/vtctldata"
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
)

// API is the main entrypoint for the vtadmin server. It implements
// vtadminpb.VTAdminServer.
type API struct {
	clusters   []*cluster.Cluster
	clusterMap map[string]*cluster.Cluster
	serv       *grpcserver.Server
	router     *mux.Router
}

// NewAPI returns a new API, configured to service the given set of clusters,
// and configured with the given gRPC and HTTP server options.
func NewAPI(clusters []*cluster.Cluster, opts grpcserver.Options, httpOpts vtadminhttp.Options) *API {
	clusterMap := make(map[string]*cluster.Cluster, len(clusters))
	for _, cluster := range clusters {
		clusterMap[cluster.ID] = cluster
	}

	sort.ClustersBy(func(c1, c2 *cluster.Cluster) bool {
		return c1.ID < c2.ID
	}).Sort(clusters)

	serv := grpcserver.New("vtadmin", opts)
	serv.Router().HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})

	router := serv.Router().PathPrefix("/api").Subrouter()

	api := &API{
		clusters:   clusters,
		clusterMap: clusterMap,
		router:     router,
		serv:       serv,
	}

	vtadminpb.RegisterVTAdminServer(serv.GRPCServer(), api)

	httpAPI := vtadminhttp.NewAPI(api)

	router.HandleFunc("/clusters", httpAPI.Adapt(vtadminhttp.GetClusters)).Name("API.GetClusters")
	router.HandleFunc("/gates", httpAPI.Adapt(vtadminhttp.GetGates)).Name("API.GetGates")
	router.HandleFunc("/keyspaces", httpAPI.Adapt(vtadminhttp.GetKeyspaces)).Name("API.GetKeyspaces")
	router.HandleFunc("/schemas", httpAPI.Adapt(vtadminhttp.GetSchemas)).Name("API.GetSchemas")
	router.HandleFunc("/tablets", httpAPI.Adapt(vtadminhttp.GetTablets)).Name("API.GetTablets")
	router.HandleFunc("/tablet/{tablet}", httpAPI.Adapt(vtadminhttp.GetTablet)).Name("API.GetTablet")
	router.HandleFunc("/vtexplain", httpAPI.Adapt(vtadminhttp.VTExplain)).Name("API.VTExplain")

	// Middlewares are executed in order of addition. Our ordering (all
	// middlewares being optional) is:
	// 	1. CORS. CORS is a special case and is applied globally, the rest are applied only to the subrouter.
	//	2. Compression
	//	3. Tracing
	middlewares := []mux.MiddlewareFunc{}

	if len(httpOpts.CORSOrigins) > 0 {
		serv.Router().Use(handlers.CORS(
			handlers.AllowCredentials(), handlers.AllowedOrigins(httpOpts.CORSOrigins)))
	}

	if !httpOpts.DisableCompression {
		middlewares = append(middlewares, handlers.CompressHandler)
	}

	if httpOpts.EnableTracing {
		middlewares = append(middlewares, vthandlers.TraceHandler)
	}

	router.Use(middlewares...)

	return api
}

// ListenAndServe starts serving this API on the configured Addr (see
// grpcserver.Options) until shutdown or irrecoverable error occurs.
func (api *API) ListenAndServe() error {
	return api.serv.ListenAndServe()
}

// GetClusters is part of the vtadminpb.VTAdminServer interface.
func (api *API) GetClusters(ctx context.Context, req *vtadminpb.GetClustersRequest) (*vtadminpb.GetClustersResponse, error) {
	span, _ := trace.NewSpan(ctx, "API.GetClusters")
	defer span.Finish()

	vcs := make([]*vtadminpb.Cluster, 0, len(api.clusters))

	for _, c := range api.clusters {
		vcs = append(vcs, &vtadminpb.Cluster{
			Id:   c.ID,
			Name: c.Name,
		})
	}

	return &vtadminpb.GetClustersResponse{
		Clusters: vcs,
	}, nil
}

// GetGates is part of the vtadminpb.VTAdminServer interface.
func (api *API) GetGates(ctx context.Context, req *vtadminpb.GetGatesRequest) (*vtadminpb.GetGatesResponse, error) {
	span, ctx := trace.NewSpan(ctx, "API.GetGates")
	defer span.Finish()

	clusters, _ := api.getClustersForRequest(req.ClusterIds)

	var (
		gates []*vtadminpb.VTGate
		wg    sync.WaitGroup
		er    concurrency.AllErrorRecorder
		m     sync.Mutex
	)

	for _, c := range clusters {
		wg.Add(1)

		go func(c *cluster.Cluster) {
			defer wg.Done()

			gs, err := c.Discovery.DiscoverVTGates(ctx, []string{})
			if err != nil {
				er.RecordError(err)
				return
			}

			m.Lock()

			for _, g := range gs {
				gates = append(gates, &vtadminpb.VTGate{
					Cell: g.Cell,
					Cluster: &vtadminpb.Cluster{
						Id:   c.ID,
						Name: c.Name,
					},
					Hostname:  g.Hostname,
					Keyspaces: g.Keyspaces,
					Pool:      g.Pool,
				})
			}

			m.Unlock()
		}(c)
	}

	wg.Wait()

	if er.HasErrors() {
		return nil, er.Error()
	}

	return &vtadminpb.GetGatesResponse{
		Gates: gates,
	}, nil
}

// GetKeyspaces is part of the vtadminpb.VTAdminServer interface.
func (api *API) GetKeyspaces(ctx context.Context, req *vtadminpb.GetKeyspacesRequest) (*vtadminpb.GetKeyspacesResponse, error) {
	span, ctx := trace.NewSpan(ctx, "API.GetKeyspaces")
	defer span.Finish()

	clusters, _ := api.getClustersForRequest(req.ClusterIds)

	var (
		keyspaces []*vtadminpb.Keyspace
		wg        sync.WaitGroup
		er        concurrency.AllErrorRecorder
		m         sync.Mutex
	)

	for _, c := range clusters {
		wg.Add(1)

		go func(c *cluster.Cluster) {
			defer wg.Done()

			if err := c.Vtctld.Dial(ctx); err != nil {
				er.RecordError(err)
				return
			}

			resp, err := c.Vtctld.GetKeyspaces(ctx, &vtctldatapb.GetKeyspacesRequest{})
			if err != nil {
				er.RecordError(err)
				return
			}

			kss := make([]*vtadminpb.Keyspace, 0, len(resp.Keyspaces))

			var (
				kwg sync.WaitGroup
				km  sync.Mutex
			)

			for _, ks := range resp.Keyspaces {
				kwg.Add(1)

				// Find all shards for each keyspace in the cluster, in parallel
				go func(c *cluster.Cluster, ks *vtctldatapb.Keyspace) {
					defer kwg.Done()
					sr, err := c.Vtctld.FindAllShardsInKeyspace(ctx, &vtctldatapb.FindAllShardsInKeyspaceRequest{
						Keyspace: ks.Name,
					})

					if err != nil {
						er.RecordError(err)
						return
					}

					km.Lock()
					kss = append(kss, &vtadminpb.Keyspace{
						Cluster:  c.ToProto(),
						Keyspace: ks,
						Shards:   sr.Shards,
					})
					km.Unlock()
				}(c, ks)
			}

			kwg.Wait()

			m.Lock()
			keyspaces = append(keyspaces, kss...)
			m.Unlock()
		}(c)
	}

	wg.Wait()

	if er.HasErrors() {
		return nil, er.Error()
	}

	return &vtadminpb.GetKeyspacesResponse{
		Keyspaces: keyspaces,
	}, nil
}

// GetSchemas is part of the vtadminpb.VTAdminServer interface.
func (api *API) GetSchemas(ctx context.Context, req *vtadminpb.GetSchemasRequest) (*vtadminpb.GetSchemasResponse, error) {
	span, ctx := trace.NewSpan(ctx, "API.GetSchemas")
	defer span.Finish()

	clusters, _ := api.getClustersForRequest(req.ClusterIds)

	var (
		schemas []*vtadminpb.Schema
		wg      sync.WaitGroup
		er      concurrency.AllErrorRecorder
		m       sync.Mutex
	)

	for _, c := range clusters {
		wg.Add(1)

		// Get schemas for the cluster
		go func(c *cluster.Cluster) {
			defer wg.Done()

			// Since tablets are per-cluster, we can fetch them once
			// and use them throughout the other waitgroups.
			tablets, err := c.GetTablets(ctx)
			if err != nil {
				er.RecordError(err)
				return
			}

			ss, err := api.getSchemas(ctx, c, tablets)
			if err != nil {
				er.RecordError(err)
				return
			}

			m.Lock()
			schemas = append(schemas, ss...)
			m.Unlock()
		}(c)
	}

	wg.Wait()

	if er.HasErrors() {
		return nil, er.Error()
	}

	return &vtadminpb.GetSchemasResponse{
		Schemas: schemas,
	}, nil
}

// getSchemas returns all of the schemas across all keyspaces in the given cluster.
func (api *API) getSchemas(ctx context.Context, c *cluster.Cluster, tablets []*vtadminpb.Tablet) ([]*vtadminpb.Schema, error) {
	if err := c.Vtctld.Dial(ctx); err != nil {
		return nil, err
	}

	resp, err := c.Vtctld.GetKeyspaces(ctx, &vtctldatapb.GetKeyspacesRequest{})
	if err != nil {
		return nil, err
	}

	var (
		schemas []*vtadminpb.Schema
		wg      sync.WaitGroup
		er      concurrency.AllErrorRecorder
		m       sync.Mutex
	)

	for _, ks := range resp.Keyspaces {
		wg.Add(1)

		// Get schemas for the cluster/keyspace
		go func(c *cluster.Cluster, ks *vtctldatapb.Keyspace) {
			defer wg.Done()

			ss, err := api.getSchemasForKeyspace(ctx, c, ks, tablets)
			if err != nil {
				er.RecordError(err)
				return
			}

			// Ignore keyspaces without schemas
			if ss == nil {
				return
			}

			m.Lock()
			schemas = append(schemas, ss)
			m.Unlock()
		}(c, ks)
	}

	wg.Wait()

	if er.HasErrors() {
		return nil, er.Error()
	}

	return schemas, nil
}

func (api *API) getSchemasForKeyspace(ctx context.Context, c *cluster.Cluster, ks *vtctldatapb.Keyspace, tablets []*vtadminpb.Tablet) (*vtadminpb.Schema, error) {
	// Choose the first serving tablet.
	var kt *vtadminpb.Tablet
	for _, t := range tablets {
		if t.Tablet.Keyspace != ks.Name || t.State != vtadminpb.Tablet_SERVING {
			continue
		}

		kt = t
	}

	// Skip schema lookup on this keyspace if there are no serving tablets.
	if kt == nil {
		return nil, nil
	}

	if err := c.Vtctld.Dial(ctx); err != nil {
		return nil, err
	}

	s, err := c.Vtctld.GetSchema(ctx, &vtctldatapb.GetSchemaRequest{
		TabletAlias: kt.Tablet.Alias,
	})

	if err != nil {
		return nil, err
	}

	// Ignore any schemas without table definitions; otherwise we return
	// a vtadminpb.Schema object with only Cluster and Keyspace defined,
	// which is not particularly useful.
	if s == nil || s.Schema == nil || len(s.Schema.TableDefinitions) == 0 {
		return nil, nil
	}

	return &vtadminpb.Schema{
		Cluster: &vtadminpb.Cluster{
			Id:   c.ID,
			Name: c.Name,
		},
		Keyspace:         ks.Name,
		TableDefinitions: s.Schema.TableDefinitions,
	}, nil
}

// GetTablet is part of the vtadminpb.VTAdminServer interface.
func (api *API) GetTablet(ctx context.Context, req *vtadminpb.GetTabletRequest) (*vtadminpb.Tablet, error) {
	span, ctx := trace.NewSpan(ctx, "API.GetTablet")
	defer span.Finish()

	span.Annotate("tablet_hostname", req.Hostname)

	clusters, ids := api.getClustersForRequest(req.ClusterIds)

	var (
		tablets []*vtadminpb.Tablet
		wg      sync.WaitGroup
		er      concurrency.AllErrorRecorder
		m       sync.Mutex
	)

	for _, c := range clusters {
		wg.Add(1)

		go func(c *cluster.Cluster) {
			defer wg.Done()

			ts, err := c.GetTablets(ctx)
			if err != nil {
				er.RecordError(err)
				return
			}

			var found []*vtadminpb.Tablet

			for _, t := range ts {
				if t.Tablet.Hostname == req.Hostname {
					found = append(found, t)
				}
			}

			m.Lock()
			tablets = append(tablets, found...)
			m.Unlock()
		}(c)
	}

	wg.Wait()

	if er.HasErrors() {
		return nil, er.Error()
	}

	switch len(tablets) {
	case 0:
		return nil, vterrors.Errorf(vtrpcpb.Code_NOT_FOUND, "%s: %s, searched clusters = %v", errors.ErrNoTablet, req.Hostname, ids)
	case 1:
		return tablets[0], nil
	}

	return nil, vterrors.Errorf(vtrpcpb.Code_NOT_FOUND, "%s: %s, searched clusters = %v", errors.ErrAmbiguousTablet, req.Hostname, ids)
}

// GetTablets is part of the vtadminpb.VTAdminServer interface.
func (api *API) GetTablets(ctx context.Context, req *vtadminpb.GetTabletsRequest) (*vtadminpb.GetTabletsResponse, error) {
	span, ctx := trace.NewSpan(ctx, "API.GetTablets")
	defer span.Finish()

	clusters, _ := api.getClustersForRequest(req.ClusterIds)

	var (
		tablets []*vtadminpb.Tablet
		wg      sync.WaitGroup
		er      concurrency.AllErrorRecorder
		m       sync.Mutex
	)

	for _, c := range clusters {
		wg.Add(1)

		go func(c *cluster.Cluster) {
			defer wg.Done()

			ts, err := c.GetTablets(ctx)
			if err != nil {
				er.RecordError(err)
				return
			}

			m.Lock()
			tablets = append(tablets, ts...)
			m.Unlock()
		}(c)
	}

	wg.Wait()

	if er.HasErrors() {
		return nil, er.Error()
	}

	return &vtadminpb.GetTabletsResponse{
		Tablets: tablets,
	}, nil
}

func (api *API) getClustersForRequest(ids []string) ([]*cluster.Cluster, []string) {
	if len(ids) == 0 {
		clusterIDs := make([]string, 0, len(api.clusters))

		for k := range api.clusterMap {
			clusterIDs = append(clusterIDs, k)
		}

		return api.clusters, clusterIDs
	}

	clusters := make([]*cluster.Cluster, 0, len(ids))

	for _, id := range ids {
		if c, ok := api.clusterMap[id]; ok {
			clusters = append(clusters, c)
		}
	}

	return clusters, ids
}

// VTExplain is part of the vtadminpb.VTAdminServer interface.
func (api *API) VTExplain(ctx context.Context, req *vtadminpb.VTExplainRequest) (*vtadminpb.VTExplainResponse, error) {
	span, ctx := trace.NewSpan(ctx, "API.VTExplain")
	defer span.Finish()

	if req.Cluster == "" {
		return nil, fmt.Errorf("%w: cluster ID is required", errors.ErrInvalidRequest)
	}

	if req.Keyspace == "" {
		return nil, fmt.Errorf("%w: keyspace name is required", errors.ErrInvalidRequest)
	}

	if req.Sql == "" {
		return nil, fmt.Errorf("%w: SQL query is required", errors.ErrInvalidRequest)
	}

	c, ok := api.clusterMap[req.Cluster]
	if !ok {
		return nil, errors.ErrUnsupportedCluster
	}

	tablet, err := c.FindTablet(ctx, func(t *vtadminpb.Tablet) bool {
		return t.Tablet.Keyspace == req.Keyspace && topo.IsInServingGraph(t.Tablet.Type) && t.Tablet.Type != topodatapb.TabletType_MASTER && t.State == vtadminpb.Tablet_SERVING
	})

	if err != nil {
		return nil, err
	}

	if err := c.Vtctld.Dial(ctx); err != nil {
		return nil, err
	}

	var (
		wg sync.WaitGroup
		er concurrency.AllErrorRecorder

		// Writes to these three variables are, in the strictest sense, unsafe.
		// However, there is one goroutine responsible for writing each of these
		// values (so, no concurrent writes), and reads are blocked on the call to
		// wg.Wait(), so we guarantee that all writes have finished before attempting
		// to read anything.
		srvVSchema string
		schema     string
		shardMap   string
	)

	wg.Add(3)

	// GetSchema
	go func(c *cluster.Cluster) {
		defer wg.Done()

		res, err := c.Vtctld.GetSchema(ctx, &vtctldatapb.GetSchemaRequest{
			TabletAlias: tablet.Tablet.Alias,
		})

		if err != nil {
			er.RecordError(err)
			return
		}

		schemas := make([]string, len(res.Schema.TableDefinitions))
		for i, td := range res.Schema.TableDefinitions {
			schemas[i] = td.Schema
		}

		schema = strings.Join(schemas, ";")
	}(c)

	// GetSrvVSchema
	go func(c *cluster.Cluster) {
		defer wg.Done()

		res, err := c.Vtctld.GetSrvVSchema(ctx, &vtctldatapb.GetSrvVSchemaRequest{
			Cell: tablet.Tablet.Alias.Cell,
		})

		if err != nil {
			er.RecordError(err)
			return
		}

		ksvs, ok := res.SrvVSchema.Keyspaces[req.Keyspace]
		if !ok {
			er.RecordError(fmt.Errorf("%w: keyspace %s", errors.ErrNoSrvVSchema, req.Keyspace))
			return
		}

		ksvsb, err := json.Marshal(&ksvs)
		if err != nil {
			er.RecordError(err)
			return
		}

		srvVSchema = fmt.Sprintf(`{"%s": %s}`, req.Keyspace, string(ksvsb))
	}(c)

	// FindAllShardsInKeyspace
	go func(c *cluster.Cluster) {
		defer wg.Done()

		ksm, err := c.Vtctld.FindAllShardsInKeyspace(ctx, &vtctldatapb.FindAllShardsInKeyspaceRequest{
			Keyspace: req.Keyspace,
		})

		if err != nil {
			er.RecordError(err)
			return
		}

		vtsm := make(map[string]*topodatapb.Shard)
		for _, s := range ksm.Shards {
			vtsm[s.Name] = s.Shard
		}

		vtsb, err := json.Marshal(&vtsm)
		if err != nil {
			er.RecordError(err)
			return
		}

		shardMap = fmt.Sprintf(`{"%s": %s}`, req.Keyspace, string(vtsb))
	}(c)

	wg.Wait()

	if er.HasErrors() {
		return nil, er.Error()
	}

	opts := &vtexplain.Options{ReplicationMode: "ROW"}

	if err := vtexplain.Init(srvVSchema, schema, shardMap, opts); err != nil {
		return nil, err
	}

	plans, err := vtexplain.Run(req.Sql)
	if err != nil {
		return nil, err
	}

	response := vtexplain.ExplainsAsText(plans)
	return &vtadminpb.VTExplainResponse{
		Response: response,
	}, nil
}