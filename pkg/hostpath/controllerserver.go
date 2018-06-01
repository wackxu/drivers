/*
Copyright 2017 The Kubernetes Authors.

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

package hostpath

import (
	"fmt"
	"os"

	"github.com/golang/glog"
	"github.com/pborman/uuid"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/container-storage-interface/spec/lib/go/csi/v0"
	"github.com/kubernetes-csi/drivers/pkg/csi-common"
	utilexec "k8s.io/utils/exec"
	"time"
)

const (
	deviceID           = "deviceID"
	provisionRoot      = "/tmp/"
	snapshotRoot       = "/tmp/"
	maxStorageCapacity = tib
)

var _ csi.ControllerServer = &controllerServer{}

type controllerServer struct {
	*csicommon.DefaultControllerServer
}

func (cs *controllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		glog.V(3).Infof("invalid create volume req: %v", req)
		return nil, err
	}

	// Check arguments
	if len(req.GetName()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Name missing in request")
	}
	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume Capabilities missing in request")
	}
	// Need to check for already existing volume name, and if found
	// check for the requested capacity and already allocated capacity
	if exVol, err := getVolumeByName(req.GetName()); err == nil {
		// Since err is nil, it means the volume with the same name already exists
		// need to check if the size of exisiting volume is the same as in new
		// request
		if exVol.VolSize >= int64(req.GetCapacityRange().GetRequiredBytes()) {
			// exisiting volume is compatible with new request and should be reused.
			// TODO (sbezverk) Do I need to make sure that RBD volume still exists?
			return &csi.CreateVolumeResponse{
				Volume: &csi.Volume{
					Id:            exVol.VolID,
					CapacityBytes: int64(exVol.VolSize),
					Attributes:    req.GetParameters(),
				},
			}, nil
		}
		return nil, status.Error(codes.AlreadyExists, fmt.Sprintf("Volume with the same name: %s but with different size already exist", req.GetName()))
	}
	// Check for maximum available capacity
	capacity := int64(req.GetCapacityRange().GetRequiredBytes())
	if capacity >= maxStorageCapacity {
		return nil, status.Errorf(codes.OutOfRange, "Requested capacity %d exceeds maximum allowed %d", capacity, maxStorageCapacity)
	}
	volumeID := uuid.NewUUID().String()
	path := provisionRoot + volumeID
	err := os.MkdirAll(path, 0777)
	if err != nil {
		glog.V(3).Infof("failed to create volume: %v", err)
		return nil, err
	}
	glog.V(4).Infof("create volume %s", path)
	hostPathVol := hostPathVolume{}
	hostPathVol.VolName = req.GetName()
	hostPathVol.VolID = volumeID
	hostPathVol.VolSize = capacity
	hostPathVol.VolPath = path
	hostPathVolumes[volumeID] = hostPathVol
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			Id:            volumeID,
			CapacityBytes: req.GetCapacityRange().GetRequiredBytes(),
			Attributes:    req.GetParameters(),
		},
	}, nil
}

func (cs *controllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {

	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		glog.V(3).Infof("invalid delete volume req: %v", req)
		return nil, err
	}
	volumeID := req.VolumeId
	glog.V(4).Infof("deleting volume %s", volumeID)
	path := provisionRoot + volumeID
	os.RemoveAll(path)
	delete(hostPathVolumes, volumeID)
	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *controllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {

	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities missing in request")
	}

	for _, cap := range req.VolumeCapabilities {
		if cap.GetAccessMode().GetMode() != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
			return &csi.ValidateVolumeCapabilitiesResponse{Supported: false, Message: ""}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{Supported: true, Message: ""}, nil
}

func (cs *controllerServer) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT); err != nil {
		glog.V(3).Infof("invalid create snapshot req: %v", req)
		return nil, err
	}

	// Check arguments
	if len(req.GetSourceVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "SourceVolumeId missing in request")
	}
	if len(req.GetName()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Name missing in request")
	}

	volumeID := req.GetSourceVolumeId()
	hostPathVolume, ok := hostPathVolumes[volumeID]
	if !ok {
		return nil, status.Error(codes.Internal, "volumeID is not exist")
	}

	snapshotID := uuid.NewUUID().String()
	createAt := time.Now().UnixNano()
	volPath := hostPathVolume.VolPath
	file := snapshotRoot + snapshotID + ".tgz"
	args := []string{"czf", file, "-C", volPath, "."}
	executor := utilexec.New()
	out, err := executor.Command("tar", args...).CombinedOutput()
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("failed create snapshot: %v: %s", err, out))
	}

	glog.V(4).Infof("create volume snapshot %s", file)
	hostPathSna := hostPathSnapshot{}
	hostPathSna.SnaName = req.GetName()
	hostPathSna.SnaID = snapshotID
	hostPathSna.VolID = volumeID
	hostPathSna.SnaPath = file
	hostPathSna.CreateAt = createAt
	hostPathSna.Status = &csi.SnapshotStatus{
		Type:    csi.SnapshotStatus_READY,
		Details: fmt.Sprint("Successfully create snapshot"),
	}

	hostPathVolumeSnapshots[snapshotID] = hostPathSna

	return &csi.CreateSnapshotResponse{
		Snapshot: &csi.Snapshot{
			Id:             hostPathSna.SnaID,
			SourceVolumeId: hostPathSna.VolID,
			CreatedAt:      hostPathSna.CreateAt,
			Status:         hostPathSna.Status,
		},
	}, nil
}

func (cs *controllerServer) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	// Check arguments
	if len(req.GetSnapshotId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Snapshot ID missing in request")
	}

	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT); err != nil {
		glog.V(3).Infof("invalid delete snapshot req: %v", req)
		return nil, err
	}
	snapshotID := req.SnapshotId
	glog.V(4).Infof("deleting volume %s", snapshotID)
	path := snapshotRoot + snapshotID + ".tgz"
	os.RemoveAll(path)
	delete(hostPathVolumeSnapshots, snapshotID)
	return &csi.DeleteSnapshotResponse{}, nil
}

func (cs *controllerServer) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	// Check arguments
	if len(req.GetSnapshotId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Snapshot ID missing in request")
	}

	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS); err != nil {
		glog.V(3).Infof("invalid list snapshot req: %v", req)
		return nil, err
	}

	snapshotID := req.SnapshotId
	snapshot := hostPathVolumeSnapshots[snapshotID]

	rsp := &csi.ListSnapshotsResponse{
		Entries: []*csi.ListSnapshotsResponse_Entry{
			{
				Snapshot: &csi.Snapshot{
					Id:             snapshot.SnaID,
					SourceVolumeId: snapshot.VolID,
					CreatedAt:      snapshot.CreateAt,
					Status:         snapshot.Status,
				},
			},
		},
	}

	return rsp, nil
}
