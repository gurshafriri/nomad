package volumewatcher

import (
	"fmt"
	"testing"

	memdb "github.com/hashicorp/go-memdb"
	cstructs "github.com/hashicorp/nomad/client/structs"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/stretchr/testify/require"
)

func TestCSI_GCVolumeClaims_Collection(t *testing.T) {
	t.Parallel()
	srv, shutdownSrv := TestServer(t, func(c *Config) { c.NumSchedulers = 0 })
	defer shutdownSrv()
	testutil.WaitForLeader(t, srv.RPC)

	state := srv.fsm.State()
	ws := memdb.NewWatchSet()
	index := uint64(100)

	// Create a client node, plugin, and volume
	node := mock.Node()
	node.Attributes["nomad.version"] = "0.11.0" // client RPCs not supported on early version
	node.CSINodePlugins = map[string]*structs.CSIInfo{
		"csi-plugin-example": {
			PluginID:                 "csi-plugin-example",
			Healthy:                  true,
			RequiresControllerPlugin: true,
			NodeInfo:                 &structs.CSINodeInfo{},
		},
	}
	node.CSIControllerPlugins = map[string]*structs.CSIInfo{
		"csi-plugin-example": {
			PluginID:                 "csi-plugin-example",
			Healthy:                  true,
			RequiresControllerPlugin: true,
			ControllerInfo: &structs.CSIControllerInfo{
				SupportsReadOnlyAttach:           true,
				SupportsAttachDetach:             true,
				SupportsListVolumes:              true,
				SupportsListVolumesAttachedNodes: false,
			},
		},
	}
	err := state.UpsertNode(99, node)
	require.NoError(t, err)
	volId0 := uuid.Generate()
	ns := structs.DefaultNamespace
	vols := []*structs.CSIVolume{{
		ID:             volId0,
		Namespace:      ns,
		PluginID:       "csi-plugin-example",
		AccessMode:     structs.CSIVolumeAccessModeMultiNodeSingleWriter,
		AttachmentMode: structs.CSIVolumeAttachmentModeFilesystem,
	}}

	err = state.CSIVolumeRegister(index, vols)
	index++
	require.NoError(t, err)
	vol, err := state.CSIVolumeByID(ws, ns, volId0)

	require.NoError(t, err)
	require.True(t, vol.ControllerRequired)
	require.Len(t, vol.ReadAllocs, 0)
	require.Len(t, vol.WriteAllocs, 0)

	// Create a job with 2 allocations
	job := mock.Job()
	job.TaskGroups[0].Volumes = map[string]*structs.VolumeRequest{
		"_": {
			Name:     "someVolume",
			Type:     structs.VolumeTypeCSI,
			Source:   volId0,
			ReadOnly: false,
		},
	}
	err = state.UpsertJob(index, job)
	index++
	require.NoError(t, err)

	alloc1 := mock.Alloc()
	alloc1.JobID = job.ID
	alloc1.NodeID = node.ID
	err = state.UpsertJobSummary(index, mock.JobSummary(alloc1.JobID))
	index++
	require.NoError(t, err)
	alloc1.TaskGroup = job.TaskGroups[0].Name

	alloc2 := mock.Alloc()
	alloc2.JobID = job.ID
	alloc2.NodeID = node.ID
	err = state.UpsertJobSummary(index, mock.JobSummary(alloc2.JobID))
	index++
	require.NoError(t, err)
	alloc2.TaskGroup = job.TaskGroups[0].Name

	err = state.UpsertAllocs(104, []*structs.Allocation{alloc1, alloc2})
	require.NoError(t, err)

	// Claim the volumes and verify the claims were set
	err = state.CSIVolumeClaim(index, ns, volId0, alloc1, structs.CSIVolumeClaimWrite)
	index++
	require.NoError(t, err)
	err = state.CSIVolumeClaim(index, ns, volId0, alloc2, structs.CSIVolumeClaimRead)
	index++
	require.NoError(t, err)
	vol, err = state.CSIVolumeByID(ws, ns, volId0)
	require.NoError(t, err)
	require.Len(t, vol.ReadAllocs, 1)
	require.Len(t, vol.WriteAllocs, 1)

	// Update both allocs as failed/terminated
	alloc1.ClientStatus = structs.AllocClientStatusFailed
	alloc2.ClientStatus = structs.AllocClientStatusFailed
	err = state.UpdateAllocsFromClient(index, []*structs.Allocation{alloc1, alloc2})
	require.NoError(t, err)

	vol, err = state.CSIVolumeDenormalize(ws, vol)
	require.NoError(t, err)

	gcClaims, nodeClaims := collectClaimsToGCImpl(vol, false)
	require.Equal(t, nodeClaims[node.ID], 2)
	require.Len(t, gcClaims, 2)
}

func TestCSI_GCVolumeClaims_Reap(t *testing.T) {
	t.Parallel()
	require := require.New(t)

	s, shutdownSrv := TestServer(t, func(c *Config) { c.NumSchedulers = 0 })
	defer shutdownSrv()
	testutil.WaitForLeader(t, s.RPC)

	node := mock.Node()
	plugin := mock.CSIPlugin()
	vol := mock.CSIVolume(plugin)
	alloc := mock.Alloc()

	cases := []struct {
		Name                                string
		Claim                               gcClaimRequest
		ClaimsCount                         map[string]int
		ControllerRequired                  bool
		ExpectedErr                         string
		ExpectedCount                       int
		ExpectedClaimsCount                 int
		ExpectedNodeDetachVolumeCount       int
		ExpectedControllerDetachVolumeCount int
		ExpectedVolumeClaimCount            int
		srv                                 *MockRPCServer
	}{
		{
			Name: "NodeDetachVolume fails",
			Claim: gcClaimRequest{
				allocID: alloc.ID,
				nodeID:  node.ID,
				mode:    structs.CSIVolumeClaimRead,
			},
			ClaimsCount:                   map[string]int{node.ID: 1},
			ControllerRequired:            true,
			ExpectedErr:                   "node plugin missing",
			ExpectedClaimsCount:           1,
			ExpectedNodeDetachVolumeCount: 1,
			srv: &MockRPCServer{
				state:                        s.State(),
				nextCSINodeDetachVolumeError: fmt.Errorf("node plugin missing"),
			},
		},
		{
			Name: "ControllerDetachVolume no controllers",
			Claim: gcClaimRequest{
				allocID: alloc.ID,
				nodeID:  node.ID,
				mode:    structs.CSIVolumeClaimRead,
			},
			ClaimsCount:        map[string]int{node.ID: 1},
			ControllerRequired: true,
			ExpectedErr: fmt.Sprintf(
				"Unknown node: %s", node.ID),
			ExpectedClaimsCount:                 0,
			ExpectedNodeDetachVolumeCount:       1,
			ExpectedControllerDetachVolumeCount: 0,
			srv: &MockRPCServer{
				state: s.State(),
			},
		},
		{
			Name: "ControllerDetachVolume node-only",
			Claim: gcClaimRequest{
				allocID: alloc.ID,
				nodeID:  node.ID,
				mode:    structs.CSIVolumeClaimRead,
			},
			ClaimsCount:                         map[string]int{node.ID: 1},
			ControllerRequired:                  false,
			ExpectedClaimsCount:                 0,
			ExpectedNodeDetachVolumeCount:       1,
			ExpectedControllerDetachVolumeCount: 0,
			ExpectedVolumeClaimCount:            1,
			srv: &MockRPCServer{
				state: s.State(),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			vol.ControllerRequired = tc.ControllerRequired
			nodeClaims, err := volumeClaimReapImpl(tc.srv, &volumeClaimReapArgs{
				vol:        vol,
				plug:       plugin,
				allocID:    tc.Claim.allocID,
				nodeID:     tc.Claim.nodeID,
				mode:       tc.Claim.mode,
				region:     "global",
				namespace:  "default",
				leaderACL:  "not-in-use",
				nodeClaims: tc.ClaimsCount,
			})
			if tc.ExpectedErr != "" {
				require.EqualError(err, tc.ExpectedErr)
			} else {
				require.NoError(err)
			}
			require.Equal(tc.ExpectedClaimsCount,
				nodeClaims[tc.Claim.nodeID], "expected claims")
			require.Equal(tc.ExpectedNodeDetachVolumeCount,
				tc.srv.countCSINodeDetachVolume, "node detach RPC count")
			require.Equal(tc.ExpectedControllerDetachVolumeCount,
				tc.srv.countCSIControllerDetachVolume, "controller detach RPC count")
			require.Equal(tc.ExpectedVolumeClaimCount,
				tc.srv.countCSIVolumeClaim, "volume claim RPC count")
		})
	}
}

type MockRPCServer struct {
	state *state.StateStore

	// mock responses for ClientCSI.NodeDetachVolume
	nextCSINodeDetachVolumeResponse *cstructs.ClientCSINodeDetachVolumeResponse
	nextCSINodeDetachVolumeError    error
	countCSINodeDetachVolume        int

	// mock responses for ClientCSI.ControllerDetachVolume
	nextCSIControllerDetachVolumeResponse *cstructs.ClientCSIControllerDetachVolumeResponse
	nextCSIControllerDetachVolumeError    error
	countCSIControllerDetachVolume        int

	// mock responses for CSI.VolumeClaim
	nextCSIVolumeClaimResponse *structs.CSIVolumeClaimResponse
	nextCSIVolumeClaimError    error
	countCSIVolumeClaim        int
}

func (srv *MockRPCServer) RPC(method string, args interface{}, reply interface{}) error {
	switch method {
	case "ClientCSI.NodeDetachVolume":
		reply = srv.nextCSINodeDetachVolumeResponse
		srv.countCSINodeDetachVolume++
		return srv.nextCSINodeDetachVolumeError
	case "ClientCSI.ControllerDetachVolume":
		reply = srv.nextCSIControllerDetachVolumeResponse
		srv.countCSIControllerDetachVolume++
		return srv.nextCSIControllerDetachVolumeError
	case "CSIVolume.Claim":
		reply = srv.nextCSIVolumeClaimResponse
		srv.countCSIVolumeClaim++
		return srv.nextCSIVolumeClaimError
	default:
		return fmt.Errorf("unexpected method %q passed to mock", method)
	}

}

func (srv *MockRPCServer) State() *state.StateStore { return srv.state }
