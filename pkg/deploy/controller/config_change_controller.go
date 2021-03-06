package controller

import (
	kapi "github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	cache "github.com/GoogleCloudPlatform/kubernetes/pkg/client/cache"
	runtime "github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
	util "github.com/GoogleCloudPlatform/kubernetes/pkg/util"

	deployapi "github.com/openshift/origin/pkg/deploy/api"
	deployutil "github.com/openshift/origin/pkg/deploy/util"

	"github.com/golang/glog"
)

// DeploymentConfigChangeController watches for changes to DeploymentConfigs and regenerates them only
// when detecting a change to the PodTemplate of a DeploymentConfig containing a ConfigChange
// trigger.
type DeploymentConfigChangeController struct {
	ChangeStrategy       changeStrategy
	NextDeploymentConfig func() *deployapi.DeploymentConfig
	DeploymentStore      cache.Store
	Codec                runtime.Codec
	// Stop is an optional channel that controls when the controller exits
	Stop <-chan struct{}
}

type changeStrategy interface {
	GenerateDeploymentConfig(namespace, name string) (*deployapi.DeploymentConfig, error)
	UpdateDeploymentConfig(namespace string, config *deployapi.DeploymentConfig) (*deployapi.DeploymentConfig, error)
}

// Run watches for config change events.
func (dc *DeploymentConfigChangeController) Run() {
	go util.Until(func() { dc.HandleDeploymentConfig() }, 0, dc.Stop)
}

// HandleDeploymentConfig handles the next DeploymentConfig change that happens.
func (dc *DeploymentConfigChangeController) HandleDeploymentConfig() {
	config := dc.NextDeploymentConfig()

	hasChangeTrigger := false
	for _, trigger := range config.Triggers {
		if trigger.Type == deployapi.DeploymentTriggerOnConfigChange {
			hasChangeTrigger = true
			break
		}
	}

	if !hasChangeTrigger {
		glog.V(4).Infof("Config has no change trigger; skipping")
		return
	}

	if config.LatestVersion == 0 {
		glog.V(4).Infof("Creating new deployment for config %v", config.Name)
		dc.generateDeployment(config, nil)
		return
	}

	latestDeploymentID := deployutil.LatestDeploymentIDForConfig(config)
	obj, exists := dc.DeploymentStore.Get(latestDeploymentID)

	if !exists {
		glog.V(4).Info("Ignoring config change due to lack of existing deployment")
		return
	}

	deployment := obj.(*kapi.ReplicationController)

	if deployedConfig, err := deployutil.DecodeDeploymentConfig(deployment, dc.Codec); err == nil {
		if deployutil.PodSpecsEqual(config.Template.ControllerTemplate.Template.Spec, deployedConfig.Template.ControllerTemplate.Template.Spec) {
			glog.V(4).Infof("Ignoring updated config %s with LatestVersion=%d because it matches deployed config %s", config.Name, config.LatestVersion, deployment.Name)
			return
		}
	} else {
		glog.V(0).Infof("Error decoding deploymentConfig from deployment %s: %v", deployment.Name, err)
	}

	dc.generateDeployment(config, deployment)
}

func (dc *DeploymentConfigChangeController) generateDeployment(config *deployapi.DeploymentConfig, deployment *kapi.ReplicationController) {
	newConfig, err := dc.ChangeStrategy.GenerateDeploymentConfig(config.Namespace, config.Name)
	if err != nil {
		glog.V(2).Infof("Error generating new version of deploymentConfig %v: %#v", config.Name, err)
		return
	}

	if deployment != nil {
		glog.V(4).Infof("Updating config %s (LatestVersion: %d -> %d) to advance existing deployment %s", config.Name, config.LatestVersion, newConfig.LatestVersion, deployment.Name)
	}

	// set the trigger details for the new deployment config
	causes := []*deployapi.DeploymentCause{}
	causes = append(causes,
		&deployapi.DeploymentCause{
			Type: deployapi.DeploymentTriggerOnConfigChange,
		})
	newConfig.Details = &deployapi.DeploymentDetails{
		Causes: causes,
	}

	// This update is atomic. If it fails because a newer resource was already persisted, that's
	// okay - we can just ignore the update for the old resource and any changes to the more
	// current config will be captured in future events.
	if _, err = dc.ChangeStrategy.UpdateDeploymentConfig(config.Namespace, newConfig); err != nil {
		glog.V(2).Infof("Error updating deploymentConfig %v: %#v", config.Name, err)
	}
}
