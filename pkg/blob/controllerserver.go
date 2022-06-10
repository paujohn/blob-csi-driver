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

package blob

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Azure/azure-sdk-for-go/services/storage/mgmt/2021-02-01/storage"
	azstorage "github.com/Azure/azure-sdk-for-go/storage"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/container-storage-interface/spec/lib/go/csi"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	"sigs.k8s.io/blob-csi-driver/pkg/util"
	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/metrics"
	azure "sigs.k8s.io/cloud-provider-azure/pkg/provider"
)

// CreateVolume provisions a volume
func (d *Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if err := d.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		klog.Errorf("invalid create volume req: %v", req)
		return nil, err
	}

	volName := req.GetName()
	if len(volName) == 0 {
		return nil, status.Error(codes.InvalidArgument, "CreateVolume Name must be provided")
	}

	if err := isValidVolumeCapabilities(req.GetVolumeCapabilities()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if acquired := d.volumeLocks.TryAcquire(volName); !acquired {
		return nil, status.Errorf(codes.Aborted, volumeOperationAlreadyExistsFmt, volName)
	}
	defer d.volumeLocks.Release(volName)

	volSizeBytes := int64(req.GetCapacityRange().GetRequiredBytes())
	requestGiB := int(util.RoundUpGiB(volSizeBytes))

	parameters := req.GetParameters()
	if parameters == nil {
		parameters = make(map[string]string)
	}
	var storageAccountType, subsID, resourceGroup, location, account, containerName, containerNamePrefix, protocol, customTags, secretName, secretNamespace, pvcNamespace string
	var isHnsEnabled *bool
	var vnetResourceGroup, vnetName, subnetName string
	var matchTags, useDataPlaneAPI bool
	// set allowBlobPublicAccess as false by default
	allowBlobPublicAccess := to.BoolPtr(false)

	// store account key to k8s secret by default
	storeAccountKey := true

	// Apply ProvisionerParameters (case-insensitive). We leave validation of
	// the values to the cloud provider.
	for k, v := range parameters {
		switch strings.ToLower(k) {
		case skuNameField:
			storageAccountType = v
		case storageAccountTypeField:
			storageAccountType = v
		case locationField:
			location = v
		case storageAccountField:
			account = v
		case subscriptionIDField:
			subsID = v
		case resourceGroupField:
			resourceGroup = v
		case containerNameField:
			containerName = v
		case containerNamePrefixField:
			containerNamePrefix = v
		case protocolField:
			protocol = v
		case tagsField:
			customTags = v
		case matchTagsField:
			matchTags = strings.EqualFold(v, trueValue)
		case secretNameField:
			secretName = v
		case secretNamespaceField:
			secretNamespace = v
		case isHnsEnabledField:
			if strings.EqualFold(v, trueValue) {
				isHnsEnabled = to.BoolPtr(true)
			}
		case storeAccountKeyField:
			if strings.EqualFold(v, falseValue) {
				storeAccountKey = false
			}
		case allowBlobPublicAccessField:
			if strings.EqualFold(v, trueValue) {
				allowBlobPublicAccess = to.BoolPtr(true)
			}
		case pvcNamespaceKey:
			pvcNamespace = v
		case pvcNameKey:
			// no op
		case pvNameKey:
			// no op
		case serverNameField:
			// no op, only used in NodeStageVolume
		case storageEndpointSuffixField:
			// no op, only used in NodeStageVolume
		case vnetResourceGroupField:
			vnetResourceGroup = v
		case vnetNameField:
			vnetName = v
		case subnetNameField:
			subnetName = v
		case mountPermissionsField:
			// only do validations here, used in NodeStageVolume, NodePublishVolume
			if v != "" {
				if _, err := strconv.ParseUint(v, 8, 32); err != nil {
					return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("invalid mountPermissions %s in storage class", v))
				}
			}
		case useDataPlaneAPIField:
			useDataPlaneAPI = strings.EqualFold(v, trueValue)
		default:
			return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("invalid parameter %q in storage class", k))
		}
	}

	if matchTags && account != "" {
		return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("matchTags must set as false when storageAccount(%s) is provided", account))
	}

	if subsID != "" && subsID != d.cloud.SubscriptionID {
		if protocol == nfs {
			return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("NFS protocol is not supported in cross subscription(%s)", subsID))
		}
		if !storeAccountKey {
			return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("storeAccountKey must set as true in cross subscription(%s)", subsID))
		}
	}

	if resourceGroup == "" {
		resourceGroup = d.cloud.ResourceGroup
	}

	if secretNamespace == "" {
		if pvcNamespace == "" {
			secretNamespace = defaultNamespace
		} else {
			secretNamespace = pvcNamespace
		}
	}

	if protocol == "" {
		protocol = fuse
	}
	if !isSupportedProtocol(protocol) {
		return nil, status.Errorf(codes.InvalidArgument, "protocol(%s) is not supported, supported protocol list: %v", protocol, supportedProtocolList)
	}

	if containerName != "" && containerNamePrefix != "" {
		return nil, status.Errorf(codes.InvalidArgument, "containerName(%s) and containerNamePrefix(%s) could not be specified together", containerName, containerNamePrefix)
	}
	if !isSupportedContainerNamePrefix(containerNamePrefix) {
		return nil, status.Errorf(codes.InvalidArgument, "containerNamePrefix(%s) can only contain lowercase letters, numbers, hyphens, and length should be less than 21", containerNamePrefix)
	}
	if protocol == ecprotocol {
		// TODO: call out to edge cache to validate sku
	}

	enableHTTPSTrafficOnly := true
	accountKind := string(storage.KindStorageV2)
	var (
		vnetResourceIDs []string
		enableNfsV3     *bool
	)
	if protocol == nfs {
		enableHTTPSTrafficOnly = false
		isHnsEnabled = to.BoolPtr(true)
		enableNfsV3 = to.BoolPtr(true)
		// set VirtualNetworkResourceIDs for storage account firewall setting
		vnetResourceID := d.getSubnetResourceID()
		klog.V(2).Infof("set vnetResourceID(%s) for NFS protocol", vnetResourceID)
		vnetResourceIDs = []string{vnetResourceID}
		if err := d.updateSubnetServiceEndpoints(ctx, vnetResourceGroup, vnetName, subnetName); err != nil {
			return nil, status.Errorf(codes.Internal, "update service endpoints failed with error: %v", err)
		}
		// NFS protocol does not need account key
		storeAccountKey = false
	}

	if strings.HasPrefix(strings.ToLower(storageAccountType), "premium") {
		accountKind = string(storage.KindBlockBlobStorage)
	}
	if IsAzureStackCloud(d.cloud) {
		accountKind = string(storage.KindStorage)
		if storageAccountType != "" && storageAccountType != string(storage.SkuNameStandardLRS) && storageAccountType != string(storage.SkuNamePremiumLRS) {
			return nil, status.Errorf(codes.InvalidArgument, fmt.Sprintf("Invalid skuName value: %s, as Azure Stack only supports %s and %s Storage Account types.", storageAccountType, storage.SkuNamePremiumLRS, storage.SkuNameStandardLRS))
		}
	}

	tags, err := util.ConvertTagsToMap(customTags)
	if err != nil {
		return nil, err
	}

	accountOptions := &azure.AccountOptions{
		Name:                      account,
		Type:                      storageAccountType,
		Kind:                      accountKind,
		SubscriptionID:            subsID,
		ResourceGroup:             resourceGroup,
		Location:                  location,
		EnableHTTPSTrafficOnly:    enableHTTPSTrafficOnly,
		VirtualNetworkResourceIDs: vnetResourceIDs,
		Tags:                      tags,
		MatchTags:                 matchTags,
		IsHnsEnabled:              isHnsEnabled,
		EnableNfsV3:               enableNfsV3,
		AllowBlobPublicAccess:     allowBlobPublicAccess,
		VNetResourceGroup:         vnetResourceGroup,
		VNetName:                  vnetName,
		SubnetName:                subnetName,
	}

	var accountKey string
	accountName := account
	secrets := req.GetSecrets()
	if len(secrets) == 0 && accountName == "" {
		if v, ok := d.volMap.Load(volName); ok {
			accountName = v.(string)
		} else {
			lockKey := fmt.Sprintf("%s%s%s%s%s", storageAccountType, accountKind, resourceGroup, location, protocol)
			// search in cache first
			cache, err := d.accountSearchCache.Get(lockKey, azcache.CacheReadTypeDefault)
			if err != nil {
				return nil, err
			}
			if cache != nil {
				accountName = cache.(string)
			} else {
				d.volLockMap.LockEntry(lockKey)
				err = wait.ExponentialBackoff(d.cloud.RequestBackoff(), func() (bool, error) {
					var retErr error
					accountName, accountKey, retErr = d.cloud.EnsureStorageAccount(ctx, accountOptions, protocol)
					if isRetriableError(retErr) {
						klog.Warningf("EnsureStorageAccount(%s) failed with error(%v), waiting for retrying", account, retErr)
						return false, nil
					}
					return true, retErr
				})
				d.volLockMap.UnlockEntry(lockKey)
				if err != nil {
					return nil, status.Errorf(codes.Internal, "ensure storage account failed with %v", err)
				}
				d.accountSearchCache.Set(lockKey, accountName)
				d.volMap.Store(volName, accountName)
			}
		}
	}

	accountOptions.Name = accountName
	if len(secrets) == 0 && useDataPlaneAPI {
		if accountKey == "" {
			if accountName, accountKey, err = d.GetStorageAccesskey(ctx, accountOptions, secrets, secretName, secretNamespace); err != nil {
				return nil, status.Errorf(codes.Internal, "failed to GetStorageAccesskey on account(%s) rg(%s), error: %v", accountOptions.Name, accountOptions.ResourceGroup, err)
			}
		}
		secrets = createStorageAccountSecret(accountName, accountKey)
	}

	validContainerName := containerName
	if validContainerName == "" {
		validContainerName = volName
		if containerNamePrefix != "" {
			validContainerName = containerNamePrefix + "-" + volName
		}
		validContainerName = getValidContainerName(validContainerName, protocol)
		parameters[containerNameField] = validContainerName
	}

	var volumeID string
	mc := metrics.NewMetricContext(blobCSIDriverName, "controller_create_volume", d.cloud.ResourceGroup, d.cloud.SubscriptionID, d.Name)
	isOperationSucceeded := false
	defer func() {
		mc.ObserveOperationWithResult(isOperationSucceeded, VolumeID, volumeID)
	}()

	klog.V(2).Infof("begin to create container(%s) on account(%s) type(%s) rg(%s) location(%s) size(%d)", validContainerName, accountName, storageAccountType, resourceGroup, location, requestGiB)
	if err := d.CreateBlobContainer(ctx, resourceGroup, accountName, validContainerName, secrets); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create container(%s) on account(%s) type(%s) rg(%s) location(%s) size(%d), error: %v", validContainerName, accountName, storageAccountType, resourceGroup, location, requestGiB, err)
	}

	if storeAccountKey && len(req.GetSecrets()) == 0 {
		if accountKey == "" {
			if accountName, accountKey, err = d.GetStorageAccesskey(ctx, accountOptions, secrets, secretName, secretNamespace); err != nil {
				return nil, status.Errorf(codes.Internal, "failed to GetStorageAccesskey on account(%s) rg(%s), error: %v", accountOptions.Name, accountOptions.ResourceGroup, err)
			}
		}

		secretName, err := setAzureCredentials(d.cloud.KubeClient, accountName, accountKey, secretNamespace)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to store storage account key: %v", err)
		}
		if secretName != "" {
			klog.V(2).Infof("store account key to k8s secret(%v) in %s namespace", secretName, secretNamespace)
		}
	}

	var uuid string
	if containerName != "" {
		// add volume name as suffix to differentiate volumeID since "containerName" is specified
		// not necessary for dynamic container name creation since volumeID already contains volume name
		uuid = volName
	}
	volumeID = fmt.Sprintf(volumeIDTemplate, resourceGroup, accountName, validContainerName, uuid, secretNamespace)
	klog.V(2).Infof("create container %s on storage account %s successfully", validContainerName, accountName)

	if useDataPlaneAPI {
		d.dataPlaneAPIVolMap.Store(volumeID, "")
		d.dataPlaneAPIVolMap.Store(accountName, "")
	}

	isOperationSucceeded = true
	// reset secretNamespace field in VolumeContext
	parameters[secretNamespaceField] = secretNamespace
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: req.GetCapacityRange().GetRequiredBytes(),
			VolumeContext: parameters,
		},
	}, nil
}

// DeleteVolume delete a volume
func (d *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	if err := d.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		return nil, fmt.Errorf("invalid delete volume req: %v", req)
	}

	if acquired := d.volumeLocks.TryAcquire(volumeID); !acquired {
		return nil, status.Errorf(codes.Aborted, volumeOperationAlreadyExistsFmt, volumeID)
	}
	defer d.volumeLocks.Release(volumeID)

	resourceGroupName, accountName, containerName, _, err := GetContainerInfo(volumeID)
	if err != nil {
		// According to CSI Driver Sanity Tester, should succeed when an invalid volume id is used
		klog.Errorf("GetContainerInfo(%s) in DeleteVolume failed with error: %v", volumeID, err)
		return &csi.DeleteVolumeResponse{}, nil
	}

	secrets := req.GetSecrets()
	if len(secrets) == 0 && d.useDataPlaneAPI(volumeID, accountName) {
		_, accountName, accountKey, _, _, err := d.GetAuthEnv(ctx, volumeID, "", nil, secrets)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "GetAuthEnv(%s) failed with %v", volumeID, err)
		}
		if accountName != "" && accountKey != "" {
			secrets = createStorageAccountSecret(accountName, accountKey)
		}
	}

	mc := metrics.NewMetricContext(blobCSIDriverName, "controller_delete_volume", d.cloud.ResourceGroup, d.cloud.SubscriptionID, d.Name)
	isOperationSucceeded := false
	defer func() {
		mc.ObserveOperationWithResult(isOperationSucceeded, VolumeID, volumeID)
	}()

	if resourceGroupName == "" {
		resourceGroupName = d.cloud.ResourceGroup
	}
	klog.V(2).Infof("deleting container(%s) rg(%s) account(%s) volumeID(%s)", containerName, resourceGroupName, accountName, volumeID)
	if err := d.DeleteBlobContainer(ctx, resourceGroupName, accountName, containerName, secrets); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to delete container(%s) under rg(%s) account(%s) volumeID(%s), error: %v", containerName, resourceGroupName, accountName, volumeID, err)
	}

	isOperationSucceeded = true
	klog.V(2).Infof("container(%s) under rg(%s) account(%s) volumeID(%s) is deleted successfully", containerName, resourceGroupName, accountName, volumeID)
	return &csi.DeleteVolumeResponse{}, nil
}

// ValidateVolumeCapabilities return the capabilities of the volume
func (d *Driver) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if err := isValidVolumeCapabilities(req.GetVolumeCapabilities()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	resourceGroupName, accountName, containerName, _, err := GetContainerInfo(volumeID)
	if err != nil {
		klog.Errorf("GetContainerInfo(%s) in ValidateVolumeCapabilities failed with error: %v", volumeID, err)
		return nil, status.Error(codes.NotFound, err.Error())
	}

	var exist bool
	secrets := req.GetSecrets()
	if len(secrets) > 0 {
		container, err := getContainerReference(containerName, secrets, d.cloud.Environment)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		exist, err = container.Exists()
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	} else {
		if resourceGroupName == "" {
			resourceGroupName = d.cloud.ResourceGroup
		}
		blobContainer, err := d.cloud.BlobClient.GetContainer(ctx, resourceGroupName, accountName, containerName)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		if blobContainer.ContainerProperties == nil {
			return nil, status.Errorf(codes.Internal, "ContainerProperties of volume(%s) is nil", volumeID)
		}
		exist = blobContainer.ContainerProperties.Deleted != nil && !*blobContainer.ContainerProperties.Deleted
	}
	if !exist {
		return nil, status.Errorf(codes.NotFound, "requested volume(%s) does not exist", volumeID)
	}
	klog.V(2).Infof("ValidateVolumeCapabilities on volume(%s) succeeded", volumeID)

	// blob driver supports all AccessModes, no need to check capabilities here
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities(),
		},
		Message: "",
	}, nil
}

func (d *Driver) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ControllerPublishVolume is not yet implemented")
}

func (d *Driver) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ControllerUnpublishVolume is not yet implemented")
}

// ControllerGetVolume get volume
func (d *Driver) ControllerGetVolume(context.Context, *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ControllerGetVolume is not yet implemented")
}

// GetCapacity returns the capacity of the total available storage pool
func (d *Driver) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	return nil, status.Error(codes.Unimplemented, "GetCapacity is not yet implemented")
}

// ListVolumes return all available volumes
func (d *Driver) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ListVolumes is not yet implemented")
}

// CreateSnapshot create snapshot
func (d *Driver) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "CreateSnapshot is not yet implemented")
}

// DeleteSnapshot delete snapshot
func (d *Driver) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "DeleteSnapshot is not yet implemented")
}

// ListSnapshots list snapshots
func (d *Driver) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ListSnapshots is not yet implemented")
}

// ControllerGetCapabilities returns the capabilities of the Controller plugin
func (d *Driver) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: d.Cap,
	}, nil
}

// ControllerExpandVolume controller expand volume
func (d *Driver) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	if req.GetCapacityRange() == nil {
		return nil, status.Error(codes.InvalidArgument, "Capacity Range missing in request")
	}

	if err := d.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_EXPAND_VOLUME); err != nil {
		return nil, fmt.Errorf("invalid expand volume req: %v", req)
	}

	volSizeBytes := int64(req.GetCapacityRange().GetRequiredBytes())
	requestGiB := int64(util.RoundUpGiB(volSizeBytes))

	if volSizeBytes > containerMaxSize {
		return nil, status.Errorf(codes.OutOfRange, "required bytes (%d) exceeds the maximum supported bytes (%d)", volSizeBytes, containerMaxSize)
	}

	klog.V(2).Infof("ControllerExpandVolume(%s) successfully, currentQuota: %d Gi", req.VolumeId, requestGiB)

	return &csi.ControllerExpandVolumeResponse{CapacityBytes: req.GetCapacityRange().GetRequiredBytes()}, nil
}

// CreateBlobContainer creates a blob container
func (d *Driver) CreateBlobContainer(ctx context.Context, resourceGroupName, accountName, containerName string, secrets map[string]string) error {
	if containerName == "" {
		return fmt.Errorf("containerName is empty")
	}
	return wait.ExponentialBackoff(d.cloud.RequestBackoff(), func() (bool, error) {
		var err error
		if len(secrets) > 0 {
			container, getErr := getContainerReference(containerName, secrets, d.cloud.Environment)
			if getErr != nil {
				return true, getErr
			}
			_, err = container.CreateIfNotExists(&azstorage.CreateContainerOptions{Access: azstorage.ContainerAccessTypePrivate})
		} else {
			blobContainer := storage.BlobContainer{
				ContainerProperties: &storage.ContainerProperties{
					PublicAccess: storage.PublicAccessNone,
				},
			}
			err = d.cloud.BlobClient.CreateContainer(ctx, resourceGroupName, accountName, containerName, blobContainer)
		}
		if err != nil {
			if strings.Contains(err.Error(), containerBeingDeletedDataplaneAPIError) ||
				strings.Contains(err.Error(), containerBeingDeletedManagementAPIError) {
				klog.Warningf("CreateContainer(%s, %s, %s) failed with error(%v), retry", resourceGroupName, accountName, containerName, err)
				return false, nil
			}
		}
		return true, err
	})
}

// DeleteBlobContainer deletes a blob container
func (d *Driver) DeleteBlobContainer(ctx context.Context, resourceGroupName, accountName, containerName string, secrets map[string]string) error {
	if containerName == "" {
		return fmt.Errorf("containerName is empty")
	}
	return wait.ExponentialBackoff(d.cloud.RequestBackoff(), func() (bool, error) {
		var err error
		if len(secrets) > 0 {
			container, getErr := getContainerReference(containerName, secrets, d.cloud.Environment)
			if getErr != nil {
				return true, getErr
			}
			_, err = container.DeleteIfExists(nil)
		} else {
			err = d.cloud.BlobClient.DeleteContainer(ctx, resourceGroupName, accountName, containerName)
		}
		if err != nil {
			if strings.Contains(err.Error(), containerBeingDeletedDataplaneAPIError) ||
				strings.Contains(err.Error(), containerBeingDeletedManagementAPIError) ||
				strings.Contains(err.Error(), statusCodeNotFound) ||
				strings.Contains(err.Error(), httpCodeNotFound) {
				klog.Warningf("delete container(%s) on account(%s) failed with error(%v), return as success", containerName, accountName, err)
				return true, nil
			}
			return false, fmt.Errorf("failed to delete container(%s) on account(%s), error: %w", containerName, accountName, err)
		}
		return true, err
	})
}

// isValidVolumeCapabilities validates the given VolumeCapability array is valid
func isValidVolumeCapabilities(volCaps []*csi.VolumeCapability) error {
	if len(volCaps) == 0 {
		return fmt.Errorf("volume capabilities missing in request")
	}
	for _, c := range volCaps {
		if c.GetBlock() != nil {
			return fmt.Errorf("block volume capability not supported")
		}
	}
	return nil
}
