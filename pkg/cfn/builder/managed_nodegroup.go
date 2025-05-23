package builder

import (
	"context"
	"errors"
	"fmt"

	gfnec2 "github.com/weaveworks/eksctl/pkg/goformation/cloudformation/ec2"
	gfneks "github.com/weaveworks/eksctl/pkg/goformation/cloudformation/eks"
	gfnt "github.com/weaveworks/eksctl/pkg/goformation/cloudformation/types"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	corev1 "k8s.io/api/core/v1"

	api "github.com/weaveworks/eksctl/pkg/apis/eksctl.io/v1alpha5"
	"github.com/weaveworks/eksctl/pkg/awsapi"
	"github.com/weaveworks/eksctl/pkg/nodebootstrap"
	instanceutils "github.com/weaveworks/eksctl/pkg/utils/instance"
	"github.com/weaveworks/eksctl/pkg/vpc"
)

// ManagedNodeGroupResourceSet defines the CloudFormation resources required for a managed nodegroup
type ManagedNodeGroupResourceSet struct {
	clusterConfig         *api.ClusterConfig
	forceAddCNIPolicy     bool
	nodeGroup             *api.ManagedNodeGroup
	launchTemplateFetcher *LaunchTemplateFetcher
	ec2API                awsapi.EC2
	vpcImporter           vpc.Importer
	bootstrapper          nodebootstrap.Bootstrapper
	*resourceSet
}

const ManagedNodeGroupResourceName = "ManagedNodeGroup"

// NewManagedNodeGroup creates a new ManagedNodeGroupResourceSet
func NewManagedNodeGroup(ec2API awsapi.EC2, cluster *api.ClusterConfig, nodeGroup *api.ManagedNodeGroup, launchTemplateFetcher *LaunchTemplateFetcher, bootstrapper nodebootstrap.Bootstrapper, forceAddCNIPolicy bool, vpcImporter vpc.Importer) *ManagedNodeGroupResourceSet {
	return &ManagedNodeGroupResourceSet{
		clusterConfig:         cluster,
		forceAddCNIPolicy:     forceAddCNIPolicy,
		nodeGroup:             nodeGroup,
		launchTemplateFetcher: launchTemplateFetcher,
		ec2API:                ec2API,
		resourceSet:           newResourceSet(),
		vpcImporter:           vpcImporter,
		bootstrapper:          bootstrapper,
	}
}

func convertToTypesValueMap(input map[string]string) map[string]*gfnt.Value {
	output := make(map[string]*gfnt.Value)
	for k, v := range input {
		output[k] = gfnt.NewString(v)
	}
	return output
}

// AddAllResources adds all required CloudFormation resources
func (m *ManagedNodeGroupResourceSet) AddAllResources(ctx context.Context) error {
	m.resourceSet.template.Description = fmt.Sprintf(
		"%s (SSH access: %v) %s",
		"EKS Managed Nodes",
		api.IsEnabled(m.nodeGroup.SSH.Allow),
		"[created by eksctl]")

	m.template.Mappings[servicePrincipalPartitionMapName] = api.Partitions.ServicePrincipalPartitionMappings()

	var nodeRole *gfnt.Value
	if m.nodeGroup.IAM.InstanceRoleARN == "" {
		if err := createRole(m.resourceSet, m.clusterConfig.IAM, m.nodeGroup.IAM, true, m.forceAddCNIPolicy); err != nil {
			return err
		}
		nodeRole = gfnt.MakeFnGetAttString(cfnIAMInstanceRoleName, "Arn")
	} else {
		nodeRole = gfnt.NewString(NormalizeARN(m.nodeGroup.IAM.InstanceRoleARN))
	}

	subnets, err := AssignSubnets(ctx, m.nodeGroup, m.clusterConfig, m.ec2API)
	if err != nil {
		return err
	}

	scalingConfig := gfneks.Nodegroup_ScalingConfig{}
	if m.nodeGroup.MinSize != nil {
		scalingConfig.MinSize = gfnt.NewInteger(*m.nodeGroup.MinSize)
	}
	if m.nodeGroup.MaxSize != nil {
		scalingConfig.MaxSize = gfnt.NewInteger(*m.nodeGroup.MaxSize)
	}
	if m.nodeGroup.DesiredCapacity != nil {
		scalingConfig.DesiredSize = gfnt.NewInteger(*m.nodeGroup.DesiredCapacity)
	}

	for k, v := range m.clusterConfig.Metadata.Tags {
		if _, exists := m.nodeGroup.Tags[k]; !exists {
			m.nodeGroup.Tags[k] = v
		}
	}

	taints, err := mapTaints(m.nodeGroup.Taints)
	if err != nil {
		return err
	}

	managedResource := &gfneks.Nodegroup{
		ClusterName:   gfnt.NewString(m.clusterConfig.Metadata.Name),
		NodegroupName: gfnt.NewString(m.nodeGroup.Name),
		ScalingConfig: &scalingConfig,
		Subnets:       subnets,
		NodeRole:      nodeRole,
		Labels:        convertToTypesValueMap(m.nodeGroup.Labels),
		Tags:          convertToTypesValueMap(m.nodeGroup.Tags),
		Taints:        taints,
	}

	if m.nodeGroup.UpdateConfig != nil {
		updateConfig := &gfneks.Nodegroup_UpdateConfig{}
		if m.nodeGroup.UpdateConfig.MaxUnavailable != nil {
			updateConfig.MaxUnavailable = gfnt.NewInteger(*m.nodeGroup.UpdateConfig.MaxUnavailable)
		}
		if m.nodeGroup.UpdateConfig.MaxUnavailablePercentage != nil {
			updateConfig.MaxUnavailablePercentage = gfnt.NewInteger(*m.nodeGroup.UpdateConfig.MaxUnavailablePercentage)
		}
		managedResource.UpdateConfig = updateConfig
	}

	if m.nodeGroup.NodeRepairConfig != nil {
		nodeRepairConfig := &gfneks.Nodegroup_NodeRepairConfig{}
		if m.nodeGroup.NodeRepairConfig.Enabled != nil {
			nodeRepairConfig.Enabled = gfnt.NewBoolean(*m.nodeGroup.NodeRepairConfig.Enabled)
		}
		managedResource.NodeRepairConfig = nodeRepairConfig
	}

	if m.nodeGroup.Spot {
		// TODO use constant from SDK
		managedResource.CapacityType = gfnt.NewString("SPOT")
	}

	isCapacityBlockEnabled := false
	if m.nodeGroup.InstanceMarketOptions != nil &&
		m.nodeGroup.InstanceMarketOptions.MarketType != nil &&
		*m.nodeGroup.InstanceMarketOptions.MarketType == "capacity-block" {
		isCapacityBlockEnabled = true
		managedResource.CapacityType = gfnt.NewString("CAPACITY_BLOCK")
	}

	if m.nodeGroup.ReleaseVersion != "" {
		managedResource.ReleaseVersion = gfnt.NewString(m.nodeGroup.ReleaseVersion)
	}

	instanceTypes := m.nodeGroup.InstanceTypeList()

	makeAMIType := func() *gfnt.Value {
		return gfnt.NewString(string(api.GetAMIType(m.nodeGroup.AMIFamily, selectManagedInstanceType(m.nodeGroup), false /* not strict, allow fallback */)))
	}

	var launchTemplate *gfneks.Nodegroup_LaunchTemplateSpecification

	if m.nodeGroup.LaunchTemplate != nil {
		launchTemplateData, err := m.launchTemplateFetcher.Fetch(ctx, m.nodeGroup.LaunchTemplate)
		if err != nil {
			return err
		}
		if err := validateLaunchTemplate(launchTemplateData, m.nodeGroup); err != nil {
			return err
		}

		launchTemplate = &gfneks.Nodegroup_LaunchTemplateSpecification{
			Id: gfnt.NewString(m.nodeGroup.LaunchTemplate.ID),
		}
		if version := m.nodeGroup.LaunchTemplate.Version; version != nil {
			launchTemplate.Version = gfnt.NewString(*version)
		}

		if launchTemplateData.ImageId == nil {
			if launchTemplateData.InstanceType == "" {
				managedResource.AmiType = makeAMIType()
			} else {
				managedResource.AmiType = gfnt.NewString(string(api.GetAMIType(m.nodeGroup.AMIFamily, string(launchTemplateData.InstanceType), false /* not strict, allow fallback */)))
			}
		}

		if launchTemplateData.InstanceType == "" {
			if isCapacityBlockEnabled {
				if len(instanceTypes) > 1 {
					return errors.New("when using capacity type CAPACITY_BLOCK please specify only one instance type")
				}
				launchTemplateData.InstanceType = ec2types.InstanceType(instanceTypes[0])
			} else {
				managedResource.InstanceTypes = gfnt.NewStringSlice(instanceTypes...)
			}
		}
	} else {
		launchTemplateData, err := m.makeLaunchTemplateData(ctx)
		if err != nil {
			return err
		}
		if launchTemplateData.ImageId == nil {
			managedResource.AmiType = makeAMIType()
		}

		if isCapacityBlockEnabled {
			if len(instanceTypes) > 1 {
				return errors.New("cannot specify multiple instance types when using capacity block")
			}
			launchTemplateData.InstanceType = gfnt.NewString(instanceTypes[0])
		} else {
			managedResource.InstanceTypes = gfnt.NewStringSlice(instanceTypes...)
		}

		ltRef := m.newResource("LaunchTemplate", &gfnec2.LaunchTemplate{
			LaunchTemplateName: gfnt.MakeFnSubString(fmt.Sprintf("${%s}", gfnt.StackName)),
			LaunchTemplateData: launchTemplateData,
		})
		launchTemplate = &gfneks.Nodegroup_LaunchTemplateSpecification{
			Id: ltRef,
		}
	}

	managedResource.LaunchTemplate = launchTemplate
	if m.clusterConfig.IsCustomEksEndpoint() {
		m.newResource(ManagedNodeGroupResourceName, addBetaManagedNodeGroupResources(managedResource, m.clusterConfig.Metadata.Name))
	} else {
		m.newResource(ManagedNodeGroupResourceName, managedResource)
	}
	return nil
}

func mapTaints(taints []api.NodeGroupTaint) ([]gfneks.Nodegroup_Taint, error) {
	var ret []gfneks.Nodegroup_Taint

	mapEffect := func(effect corev1.TaintEffect) ekstypes.TaintEffect {
		switch effect {
		case corev1.TaintEffectNoSchedule:
			return ekstypes.TaintEffectNoSchedule
		case corev1.TaintEffectPreferNoSchedule:
			return ekstypes.TaintEffectPreferNoSchedule
		case corev1.TaintEffectNoExecute:
			return ekstypes.TaintEffectNoExecute
		default:
			return ""
		}
	}

	for _, t := range taints {
		effect := mapEffect(t.Effect)
		if effect == "" {
			return nil, fmt.Errorf("unexpected taint effect: %v", t.Effect)
		}
		ret = append(ret, gfneks.Nodegroup_Taint{
			Key:    gfnt.NewString(t.Key),
			Value:  gfnt.NewString(t.Value),
			Effect: gfnt.NewString(string(effect)),
		})
	}
	return ret, nil
}

func selectManagedInstanceType(ng *api.ManagedNodeGroup) string {
	if len(ng.InstanceTypes) > 0 {
		for _, instanceType := range ng.InstanceTypes {
			if instanceutils.IsGPUInstanceType(instanceType) {
				return instanceType
			}
		}
		return ng.InstanceTypes[0]
	}
	return ng.InstanceType
}

func validateLaunchTemplate(launchTemplateData *ec2types.ResponseLaunchTemplateData, ng *api.ManagedNodeGroup) error {
	const mngFieldName = "managedNodeGroup"

	if launchTemplateData.InstanceType == "" {
		if len(ng.InstanceTypes) == 0 {
			return fmt.Errorf("instance type must be set in the launch template if %s.instanceTypes is not specified", mngFieldName)
		}
	} else if len(ng.InstanceTypes) > 0 {
		return fmt.Errorf("instance type must not be set in the launch template if %s.instanceTypes is specified", mngFieldName)
	}

	// Custom AMI
	if launchTemplateData.ImageId != nil {
		if launchTemplateData.UserData == nil {
			return errors.New("node bootstrapping script (UserData) must be set when using a custom AMI")
		}
		notSupportedErr := func(fieldName string) error {
			return fmt.Errorf("cannot set %s.%s when launchTemplate.ImageId is set", mngFieldName, fieldName)

		}
		if ng.AMI != "" {
			return notSupportedErr("ami")
		}
		if ng.ReleaseVersion != "" {
			return notSupportedErr("releaseVersion")
		}
	}

	if launchTemplateData.IamInstanceProfile != nil && launchTemplateData.IamInstanceProfile.Arn != nil {
		return errors.New("IAM instance profile must not be set in the launch template")
	}

	return nil
}

// RenderJSON implements the ResourceSet interface
func (m *ManagedNodeGroupResourceSet) RenderJSON() ([]byte, error) {
	return m.resourceSet.renderJSON()
}

// WithIAM implements the ResourceSet interface
func (m *ManagedNodeGroupResourceSet) WithIAM() bool {
	// eksctl does not support passing pre-created IAM instance roles to Managed Nodes,
	// so the IAM capability is always required
	return true
}

// WithNamedIAM implements the ResourceSet interface
func (m *ManagedNodeGroupResourceSet) WithNamedIAM() bool {
	return m.nodeGroup.IAM.InstanceRoleName != ""
}
