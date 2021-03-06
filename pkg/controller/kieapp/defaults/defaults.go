package defaults

//go:generate go run .packr/packr.go

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"os"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/gobuffalo/packr/v2"
	"github.com/imdario/mergo"
	v1 "github.com/kiegroup/kie-cloud-operator/pkg/apis/app/v1"
	"github.com/kiegroup/kie-cloud-operator/pkg/controller/kieapp/constants"
	"github.com/kiegroup/kie-cloud-operator/pkg/controller/kieapp/logs"
	"github.com/kiegroup/kie-cloud-operator/pkg/controller/kieapp/shared"
	"github.com/kiegroup/kie-cloud-operator/version"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

var log = logs.GetLogger("kieapp.defaults")

// GetEnvironment returns an Environment from merging the common config and the config
// related to the environment set in the KieApp definition
func GetEnvironment(cr *v1.KieApp, service v1.PlatformService) (v1.Environment, error) {
	envTemplate, err := getEnvTemplate(cr)
	if err != nil {
		return v1.Environment{}, err
	}
	var common v1.Environment
	yamlBytes, err := loadYaml(service, "common.yaml", cr.Spec.CommonConfig.Version, cr.Namespace, envTemplate)
	if err != nil {
		return v1.Environment{}, err
	}
	err = yaml.Unmarshal(yamlBytes, &common)
	if err != nil {
		return v1.Environment{}, err
	}
	var env v1.Environment
	yamlBytes, err = loadYaml(service, fmt.Sprintf("envs/%s.yaml", cr.Spec.Environment), cr.Spec.CommonConfig.Version, cr.Namespace, envTemplate)
	if err != nil {
		return v1.Environment{}, err
	}
	err = yaml.Unmarshal(yamlBytes, &env)
	if err != nil {
		return v1.Environment{}, err
	}
	if cr.Spec.Objects.SmartRouter == nil {
		env.SmartRouter.Omit = true
	}

	mergedEnv, err := merge(common, env)
	if err != nil {
		return v1.Environment{}, err
	}
	if mergedEnv.SmartRouter.Omit {
		// remove router env vars from kieserver DCs
		for _, server := range mergedEnv.Servers {
			for _, dc := range server.DeploymentConfigs {
				newSlice := []corev1.EnvVar{}
				for _, envvar := range dc.Spec.Template.Spec.Containers[0].Env {
					if envvar.Name != "KIE_SERVER_ROUTER_SERVICE" && envvar.Name != "KIE_SERVER_ROUTER_PORT" && envvar.Name != "KIE_SERVER_ROUTER_PROTOCOL" {
						newSlice = append(newSlice, envvar)
					}
				}
				dc.Spec.Template.Spec.Containers[0].Env = newSlice
			}
		}
	}

	mergedEnv, err = mergeDB(service, cr, mergedEnv, envTemplate)
	if err != nil {
		return v1.Environment{}, err
	}
	mergedEnv, err = mergeJms(service, cr, mergedEnv, envTemplate)
	if err != nil {
		return v1.Environment{}, err
	}
	return mergedEnv, nil
}

func mergeDB(service v1.PlatformService, cr *v1.KieApp, env v1.Environment, envTemplate v1.EnvTemplate) (v1.Environment, error) {
	dbEnvs := make(map[v1.DatabaseType]v1.Environment)
	for i := range env.Servers {
		kieServerSet := envTemplate.Servers[i]
		if kieServerSet.Database.Type == "" {
			continue
		}
		dbType := kieServerSet.Database.Type
		if _, loadedDB := dbEnvs[dbType]; !loadedDB {
			yamlBytes, err := loadYaml(service, fmt.Sprintf("dbs/%s.yaml", dbType), cr.Spec.CommonConfig.Version, cr.Namespace, envTemplate)
			if err != nil {
				return v1.Environment{}, err
			}
			var dbEnv v1.Environment
			err = yaml.Unmarshal(yamlBytes, &dbEnv)
			if err != nil {
				return v1.Environment{}, err
			}
			dbEnvs[dbType] = dbEnv
		}
		dbServer, found := findCustomObjectByName(env.Servers[i], dbEnvs[dbType].Servers)
		if found {
			env.Servers[i] = mergeCustomObject(env.Servers[i], dbServer)
		}
	}
	return env, nil
}

func mergeJms(service v1.PlatformService, cr *v1.KieApp, env v1.Environment, envTemplate v1.EnvTemplate) (v1.Environment, error) {
	var jmsEnv v1.Environment
	for i := range env.Servers {
		kieServerSet := envTemplate.Servers[i]

		isJmsEnabled := kieServerSet.Jms.EnableIntegration

		if isJmsEnabled {
			yamlBytes, err := loadYaml(service, fmt.Sprintf("jms/activemq-jms-config.yaml"), cr.Spec.CommonConfig.Version, cr.Namespace, envTemplate)
			if err != nil {
				return v1.Environment{}, err
			}
			err = yaml.Unmarshal(yamlBytes, &jmsEnv)
			if err != nil {
				return v1.Environment{}, err
			}
		}

		jmsServer, found := findCustomObjectByName(env.Servers[i], jmsEnv.Servers)
		if found {
			env.Servers[i] = mergeCustomObject(env.Servers[i], jmsServer)
		}
	}
	return env, nil
}

func findCustomObjectByName(template v1.CustomObject, objects []v1.CustomObject) (v1.CustomObject, bool) {
	for i := range objects {
		if len(objects[i].DeploymentConfigs) == 0 || len(template.DeploymentConfigs) == 0 {
			return v1.CustomObject{}, false
		}
		if objects[i].DeploymentConfigs[0].ObjectMeta.Name == template.DeploymentConfigs[0].ObjectMeta.Name {
			return objects[i], true
		}
	}
	return v1.CustomObject{}, false
}

func getEnvTemplate(cr *v1.KieApp) (v1.EnvTemplate, error) {
	setAppConstants(&cr.Spec.CommonConfig)
	if !checkVersion(cr.Spec.CommonConfig.Version) {
		return v1.EnvTemplate{}, fmt.Errorf("Product version %s is not supported by this Operator, %s", cr.Spec.CommonConfig.Version, version.Version)
	}
	// set default values for go template where not provided
	config := &cr.Spec.CommonConfig
	if len(config.ImageTag) == 0 {
		config.ImageTag = constants.VersionConstants[config.Version].ImageStreamTag
	}
	if config.ApplicationName == "" {
		config.ApplicationName = cr.Name
	}
	isTrialEnv := strings.HasSuffix(string(cr.Spec.Environment), constants.TrialEnvSuffix)
	setPasswords(config, isTrialEnv)

	serversConfig, err := getServersConfig(cr, config)
	if err != nil {
		return v1.EnvTemplate{}, err
	}
	envTemplate := v1.EnvTemplate{
		CommonConfig: config,
		Console:      getConsoleTemplate(cr),
		Servers:      serversConfig,
		SmartRouter:  getSmartRouterTemplate(cr),
		Constants:    *getTemplateConstants(cr.Spec.Environment, cr.Spec.CommonConfig.Version),
	}
	if err := configureAuth(cr, &envTemplate); err != nil {
		log.Error("unable to setup authentication: ", err)
		return envTemplate, err
	}

	return envTemplate, nil
}

func getTemplateConstants(env v1.EnvironmentType, productVersion string) *v1.TemplateConstants {
	c := constants.TemplateConstants.DeepCopy()
	c.Major, c.Minor, c.Patch = MajorMinorPatch(productVersion)
	if envConstants, found := constants.EnvironmentConstants[env]; found {
		c.Product = envConstants.App.Product
		c.MavenRepo = envConstants.App.MavenRepo
	}
	if versionConstants, found := constants.VersionConstants[productVersion]; found {
		c.BrokerImage = versionConstants.BrokerImage
		c.BrokerImageTag = versionConstants.BrokerImageTag
		c.DatagridImage = versionConstants.DatagridImage
		c.DatagridImageTag = versionConstants.DatagridImageTag
	}
	return c
}

func getConsoleTemplate(cr *v1.KieApp) v1.ConsoleTemplate {
	envConstants, hasEnv := constants.EnvironmentConstants[cr.Spec.Environment]
	template := v1.ConsoleTemplate{}
	if !hasEnv {
		return template
	}
	if cr.Spec.Objects.Console.KeystoreSecret == "" {
		template.KeystoreSecret = fmt.Sprintf(constants.KeystoreSecret, strings.Join([]string{cr.Spec.CommonConfig.ApplicationName, "businesscentral"}, "-"))
	} else {
		template.KeystoreSecret = cr.Spec.Objects.Console.KeystoreSecret
	}
	// Set replicas
	envReplicas := v1.Replicas{}
	if hasEnv {
		envReplicas = envConstants.Replica.Console
	}
	replicas, denyScale := setReplicas(cr.Spec.Objects.Console.KieAppObject, envReplicas, hasEnv)
	if denyScale {
		cr.Spec.Objects.Console.Replicas = Pint32(replicas)
	}
	template.Replicas = replicas

	template.Name = envConstants.App.Prefix
	template.ImageName = envConstants.App.ImageName

	return template
}

func getSmartRouterTemplate(cr *v1.KieApp) v1.SmartRouterTemplate {
	envConstants, hasEnv := constants.EnvironmentConstants[cr.Spec.Environment]
	template := v1.SmartRouterTemplate{}
	if cr.Spec.Objects.SmartRouter != nil {
		if !hasEnv {
			return template
		}
		if cr.Spec.Objects.SmartRouter.KeystoreSecret == "" {
			template.KeystoreSecret = fmt.Sprintf(constants.KeystoreSecret, strings.Join([]string{cr.Spec.CommonConfig.ApplicationName, "smartrouter"}, "-"))
		} else {
			template.KeystoreSecret = cr.Spec.Objects.SmartRouter.KeystoreSecret
		}

		if cr.Spec.Objects.SmartRouter.Protocol == "" {
			template.Protocol = constants.SmartRouterProtocol
		} else {
			template.Protocol = cr.Spec.Objects.SmartRouter.Protocol
		}

		template.UseExternalRoute = cr.Spec.Objects.SmartRouter.UseExternalRoute

		// Set replicas
		envReplicas := v1.Replicas{}
		if hasEnv {
			envReplicas = envConstants.Replica.SmartRouter
		}
		replicas, denyScale := setReplicas(cr.Spec.Objects.SmartRouter.KieAppObject, envReplicas, hasEnv)
		if denyScale {
			cr.Spec.Objects.SmartRouter.Replicas = Pint32(replicas)
		}
		template.Replicas = replicas
	}

	return template
}

func setReplicas(object v1.KieAppObject, replicaConstant v1.Replicas, hasEnv bool) (replicas int32, denyScale bool) {
	if object.Replicas != nil {
		if hasEnv && replicaConstant.DenyScale && *object.Replicas != replicaConstant.Replicas {
			log.Warnf("scaling not allowed for this environment, setting to default of %d", replicaConstant.Replicas)
			return replicaConstant.Replicas, true
		}
		return *object.Replicas, false
	}
	if hasEnv {
		return replicaConstant.Replicas, false
	}
	log.Warnf("no replicas settings for this environment, defaulting to %d", replicas)
	return int32(1), denyScale
}

// serverSortBlanks moves blank names to the end
func serverSortBlanks(serverSets []v1.KieServerSet) []v1.KieServerSet {
	var newSets []v1.KieServerSet
	// servers with existing names should be placed in front
	for index := range serverSets {
		if serverSets[index].Name != "" {
			newSets = append(newSets, serverSets[index])
		}
	}
	// servers without names should be at the end
	for index := range serverSets {
		if serverSets[index].Name == "" {
			newSets = append(newSets, serverSets[index])
		}
	}
	if len(newSets) != len(serverSets) {
		log.Error("slice lengths aren't equal, returning server sets w/o blank names sorted")
		return serverSets
	}
	return newSets
}

// Returns the templates to use depending on whether the spec was defined with a common configuration
// or a specific one.
func getServersConfig(cr *v1.KieApp, commonConfig *v1.CommonConfig) ([]v1.ServerTemplate, error) {
	var servers []v1.ServerTemplate
	if len(cr.Spec.Objects.Servers) == 0 {
		cr.Spec.Objects.Servers = []v1.KieServerSet{{}}
	}
	cr.Spec.Objects.Servers = serverSortBlanks(cr.Spec.Objects.Servers)
	product := GetProduct(cr.Spec.Environment)
	usedNames := map[string]bool{}
	unsetNames := 0
	for index := range cr.Spec.Objects.Servers {
		serverSet := &cr.Spec.Objects.Servers[index]
		if serverSet.Deployments == nil {
			serverSet.Deployments = Pint(constants.DefaultKieDeployments)
		}
		if serverSet.Name == "" {
			for i := 0; i < len(cr.Spec.Objects.Servers); i++ {
				serverSetName := getKieSetName(cr.Spec.CommonConfig.ApplicationName, serverSet.Name, unsetNames)
				if !usedNames[serverSetName] {
					serverSet.Name = serverSetName
					break
				}
				unsetNames++
			}
		}
		for i := 0; i < *serverSet.Deployments; i++ {
			name := getKieDeploymentName(cr.Spec.CommonConfig.ApplicationName, serverSet.Name, unsetNames, i)
			if usedNames[name] {
				return []v1.ServerTemplate{}, fmt.Errorf("duplicate kieserver name %s", name)
			}
			usedNames[name] = true
			template := v1.ServerTemplate{
				KieName:        name,
				KieServerID:    name,
				Build:          getBuildConfig(product, commonConfig, serverSet.Build),
				KeystoreSecret: serverSet.KeystoreSecret,
			}
			if serverSet.ID != "" {
				template.KieServerID = serverSet.ID
			}
			if serverSet.Build != nil {
				if *serverSet.Deployments > 1 {
					return []v1.ServerTemplate{}, fmt.Errorf("Cannot request %v deployments for a build", *serverSet.Deployments)
				}
				template.From = corev1.ObjectReference{
					Kind:      "ImageStreamTag",
					Name:      fmt.Sprintf("%s-kieserver:latest", commonConfig.ApplicationName),
					Namespace: "",
				}
			} else {
				template.From = getDefaultKieServerImage(product, commonConfig, serverSet.From)
			}

			// Set replicas
			envConstants, hasEnv := constants.EnvironmentConstants[cr.Spec.Environment]
			envReplicas := v1.Replicas{}
			if hasEnv {
				envReplicas = envConstants.Replica.Server
			}
			replicas, denyScale := setReplicas(serverSet.KieAppObject, envReplicas, hasEnv)
			if denyScale {
				serverSet.Replicas = Pint32(replicas)
			}
			template.Replicas = replicas

			// if, SmartRouter object is nil, ignore it
			// get smart router protocol configuration
			if cr.Spec.Objects.SmartRouter != nil {
				if cr.Spec.Objects.SmartRouter.Protocol == "" {
					template.SmartRouter.Protocol = constants.SmartRouterProtocol
				} else {
					template.SmartRouter.Protocol = cr.Spec.Objects.SmartRouter.Protocol
				}
			}

			dbConfig, err := getDatabaseConfig(cr.Spec.Environment, serverSet.Database)
			if err != nil {
				return servers, err
			}
			if dbConfig != nil {
				template.Database = *dbConfig
			}

			jmsConfig, err := getJmsConfig(cr.Spec.Environment, serverSet.Jms)
			if err != nil {
				return servers, err
			}
			if jmsConfig != nil {
				template.Jms = *jmsConfig
			}

			instanceTemplate := template.DeepCopy()
			if instanceTemplate.KeystoreSecret == "" {
				instanceTemplate.KeystoreSecret = fmt.Sprintf(constants.KeystoreSecret, instanceTemplate.KieName)
			}
			servers = append(servers, *instanceTemplate)
		}
	}
	return servers, nil
}

// GetServerSet retrieves to correct ServerSet for processing and the DeploymentName
func GetServerSet(cr *v1.KieApp, requestedIndex int) (serverSet v1.KieServerSet, kieName string) {
	count := 0
	unnamedSets := 0
	for _, thisServerSet := range cr.Spec.Objects.Servers {
		for relativeIndex := 0; relativeIndex < *thisServerSet.Deployments; relativeIndex++ {
			if count == requestedIndex {
				serverSet = thisServerSet
				kieName = getKieDeploymentName(cr.Spec.CommonConfig.ApplicationName, serverSet.Name, unnamedSets, relativeIndex)
				return
			}
			count++
		}
		if thisServerSet.Name == "" {
			unnamedSets++
		}
	}
	return
}

// ConsolidateObjects construct all CustomObjects prior to creation
func ConsolidateObjects(env v1.Environment, cr *v1.KieApp) v1.Environment {
	env.Console = ConstructObject(env.Console, cr.Spec.Objects.Console.KieAppObject)
	if cr.Spec.Objects.SmartRouter != nil {
		env.SmartRouter = ConstructObject(env.SmartRouter, cr.Spec.Objects.SmartRouter.KieAppObject)
	}
	for index := range env.Servers {
		serverSet, _ := GetServerSet(cr, index)
		env.Servers[index] = ConstructObject(env.Servers[index], serverSet.KieAppObject)
	}
	return env
}

// ConstructObject returns an object after merging the environment object and the one defined in the CR
func ConstructObject(object v1.CustomObject, appObject v1.KieAppObject) v1.CustomObject {
	for dcIndex, dc := range object.DeploymentConfigs {
		for containerIndex, c := range dc.Spec.Template.Spec.Containers {
			c.Env = shared.EnvOverride(c.Env, appObject.Env)
			err := mergo.Merge(&c.Resources, appObject.Resources, mergo.WithOverride)
			if err != nil {
				log.Error("Error merging interfaces. ", err)
			}
			dc.Spec.Template.Spec.Containers[containerIndex] = c
		}
		object.DeploymentConfigs[dcIndex] = dc
	}
	return object
}

// getKieSetName aids in server indexing, depending on number of deployments and sets
func getKieSetName(applicationName string, setName string, arrayIdx int) string {
	return getKieDeploymentName(applicationName, setName, arrayIdx, 0)
}

func getKieDeploymentName(applicationName string, setName string, arrayIdx, deploymentsIdx int) string {
	name := setName
	if name == "" {
		name = fmt.Sprintf("%v-kieserver%v", applicationName, getKieSetIndex(arrayIdx, deploymentsIdx))
	} else {
		name = fmt.Sprintf("%v%v", setName, getKieSetIndex(0, deploymentsIdx))
	}
	return name
}

func getKieSetIndex(arrayIdx, deploymentsIdx int) string {
	var name bytes.Buffer
	if arrayIdx > 0 {
		name.WriteString(fmt.Sprintf("%d", arrayIdx+1))
	}
	if deploymentsIdx > 0 {
		name.WriteString(fmt.Sprintf("-%d", deploymentsIdx+1))
	}
	return name.String()
}

func getBuildConfig(product string, config *v1.CommonConfig, build *v1.KieAppBuildObject) v1.BuildTemplate {
	if build == nil {
		return v1.BuildTemplate{}
	}
	buildTemplate := v1.BuildTemplate{
		GitSource:                    build.GitSource,
		GitHubWebhookSecret:          getWebhookSecret(v1.GitHubWebhook, build.Webhooks),
		GenericWebhookSecret:         getWebhookSecret(v1.GenericWebhook, build.Webhooks),
		KieServerContainerDeployment: build.KieServerContainerDeployment,
		MavenMirrorURL:               build.MavenMirrorURL,
		ArtifactDir:                  build.ArtifactDir,
	}
	buildTemplate.From = getDefaultKieServerImage(product, config, build.From)
	return buildTemplate
}

func getDefaultKieServerImage(product string, config *v1.CommonConfig, from *corev1.ObjectReference) corev1.ObjectReference {
	if from != nil {
		return *from
	}
	imageName := fmt.Sprintf("%s%s-kieserver-openshift:%s", product, getMinorImageVersion(config.Version), config.ImageTag)
	return corev1.ObjectReference{
		Kind:      "ImageStreamTag",
		Name:      imageName,
		Namespace: constants.ImageStreamNamespace,
	}
}

func getDatabaseConfig(environment v1.EnvironmentType, database *v1.DatabaseObject) (*v1.DatabaseObject, error) {
	envConstants := constants.EnvironmentConstants[environment]
	if envConstants == nil {
		return nil, nil
	}
	defaultDB := envConstants.Database
	if database == nil {
		return defaultDB, nil
	}

	if database.Type == v1.DatabaseExternal && database.ExternalConfig == nil {
		return nil, fmt.Errorf("external database configuration is mandatory for external database type")
	}
	if database.Size == "" && defaultDB != nil {
		resultDB := *database.DeepCopy()
		resultDB.Size = defaultDB.Size
		return &resultDB, nil
	}
	return database, nil
}

func getJmsConfig(environment v1.EnvironmentType, jms *v1.KieAppJmsObject) (*v1.KieAppJmsObject, error) {

	envConstants := constants.EnvironmentConstants[environment]
	if envConstants == nil || jms == nil || !jms.EnableIntegration {
		return nil, nil
	}

	t := true
	if jms.Executor == nil {
		jms.Executor = &t
	}
	if jms.AuditTransacted == nil {
		jms.AuditTransacted = &t
	}

	// if enabled, prepare the default values
	defaultJms := &v1.KieAppJmsObject{
		QueueExecutor: "queue/KIE.SERVER.EXECUTOR",
		QueueRequest:  "queue/KIE.SERVER.REQUEST",
		QueueResponse: "queue/KIE.SERVER.RESPONSE",
		QueueSignal:   "queue/KIE.SERVER.SIGNAL",
		QueueAudit:    "queue/KIE.SERVER.AUDIT",
		Username:      "user" + string(shared.GeneratePassword(4)),
		Password:      string(shared.GeneratePassword(8)),
	}

	queuesList := []string{
		getDefaultQueue(*jms.Executor, defaultJms.QueueExecutor, jms.QueueExecutor),
		getDefaultQueue(true, defaultJms.QueueRequest, jms.QueueRequest),
		getDefaultQueue(true, defaultJms.QueueResponse, jms.QueueResponse),
		getDefaultQueue(jms.EnableSignal, defaultJms.QueueSignal, jms.QueueSignal),
		getDefaultQueue(jms.EnableAudit, defaultJms.QueueAudit, jms.QueueAudit),
	}

	// clean empty values
	for i, queue := range queuesList {
		if queue == "" {
			queuesList = append(queuesList[:i], queuesList[i+1:]...)
			break
		}
	}
	defaultJms.AMQQueues = strings.Join(queuesList[:], ", ")

	// merge the defaultJms into jms, preserving the values set by cr
	mergo.Merge(jms, defaultJms)

	return jms, nil
}

func getDefaultQueue(append bool, defaultJmsQueue string, jmsQueue string) string {
	if append {
		if jmsQueue == "" {
			return defaultJmsQueue
		}
		return jmsQueue
	}
	return ""
}

func setPasswords(config *v1.CommonConfig, isTrialEnv bool) {
	passwords := []*string{
		&config.KeyStorePassword,
		&config.AdminPassword,
		&config.DBPassword,
		&config.AMQPassword,
		&config.AMQClusterPassword,
		&config.ControllerPassword,
		&config.MavenPassword,
		&config.ServerPassword}

	for i := range passwords {
		if len(*passwords[i]) != 0 {
			continue
		}
		if isTrialEnv {
			*passwords[i] = constants.DefaultPassword
		} else {
			*passwords[i] = string(shared.GeneratePassword(8))
		}
	}
}

func getWebhookSecret(webhookType v1.WebhookType, webhooks []v1.WebhookSecret) string {
	for _, webhook := range webhooks {
		if webhook.Type == webhookType {
			return webhook.Secret
		}
	}
	return string(shared.GeneratePassword(8))
}

// important to parse template first with this function, before unmarshalling into object
func loadYaml(service v1.PlatformService, filename, productVersion, namespace string, env v1.EnvTemplate) ([]byte, error) {
	filename = strings.Join([]string{productVersion, filename}, "/")
	if _, _, useEmbedded := UseEmbeddedFiles(service); useEmbedded {
		box := packr.New("config", "../../../../config")
		if !box.HasDir(productVersion) {
			return nil, fmt.Errorf("Product version %s configs are not available in this Operator, %s", productVersion, version.Version)
		}
		if box.Has(filename) {
			yamlString, err := box.FindString(filename)
			if err != nil {
				return nil, err
			}
			return parseTemplate(env, yamlString), nil
		}
		return nil, fmt.Errorf("%s does not exist, '%s' KieApp not deployed", filename, env.ApplicationName)
	}

	cmName, file := convertToConfigMapName(filename)
	configMap := &corev1.ConfigMap{}
	err := service.Get(context.TODO(), types.NamespacedName{Name: cmName, Namespace: namespace}, configMap)
	if err != nil {
		return nil, fmt.Errorf("%s/%s ConfigMap not yet accessible, '%s' KieApp not deployed. Retrying... ", namespace, cmName, env.ApplicationName)
	}
	log.Debugf("Reconciling '%s' KieApp with %s from ConfigMap '%s'", env.ApplicationName, file, cmName)
	return parseTemplate(env, configMap.Data[file]), nil
}

func parseTemplate(env v1.EnvTemplate, objYaml string) []byte {
	var b bytes.Buffer

	tmpl, err := template.New(env.ApplicationName).Delims("[[", "]]").Parse(objYaml)
	if err != nil {
		log.Error("Error creating new Go template. ", err)
	}

	// template replacement
	err = tmpl.Execute(&b, env)
	if err != nil {
		log.Error("Error applying Go template. ", err)
	}

	return b.Bytes()
}

func convertToConfigMapName(filename string) (configMapName, file string) {
	name := constants.ConfigMapPrefix
	result := strings.Split(filename, "/")
	if len(result) > 1 {
		for i := 0; i < len(result)-1; i++ {
			name = strings.Join([]string{name, result[i]}, "-")
		}
	}
	return name, result[len(result)-1]
}

// ConfigMapsFromFile reads the files under the config folder and creates
// configmaps in the given namespace. It sets OwnerRef to operator deployment.
func ConfigMapsFromFile(myDep *appsv1.Deployment, ns string, scheme *runtime.Scheme) []corev1.ConfigMap {
	box := packr.New("config", "../../../../config")
	cmList := map[string][]map[string]string{}
	for _, filename := range box.List() {
		s, err := box.FindString(filename)
		if err != nil {
			log.Error("Error finding file with packr. ", err)
		}
		cmData := map[string]string{}
		cmName, file := convertToConfigMapName(filename)
		cmData[file] = s
		cmList[cmName] = append(cmList[cmName], cmData)
	}
	var configMaps []corev1.ConfigMap
	for cmName, dataSlice := range cmList {
		cmData := map[string]string{}
		for _, dataList := range dataSlice {
			for name, data := range dataList {
				cmData[name] = data
			}
		}
		cm := corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: ns,
				Annotations: map[string]string{
					v1.SchemeGroupVersion.Group: version.Version,
				},
			},
			Data: cmData,
		}

		cm.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ConfigMap"))
		err := controllerutil.SetControllerReference(myDep, &cm, scheme)
		if err != nil {
			log.Error("Error setting controller reference. ", err)
		}
		for index := range cm.OwnerReferences {
			cm.OwnerReferences[index].BlockOwnerDeletion = nil
		}
		configMaps = append(configMaps, cm)
	}
	return configMaps
}

// UseEmbeddedFiles checks environment variables WATCH_NAMESPACE & OPERATOR_NAME
func UseEmbeddedFiles(service v1.PlatformService) (opName string, depNameSpace string, useEmbedded bool) {
	namespace := os.Getenv(constants.NameSpaceEnv)
	name := os.Getenv(constants.OpNameEnv)
	if service.IsMockService() || namespace == "" || name == "" {
		return name, namespace, true
	}
	return name, namespace, false
}

// setAppConstants sets the application-related constants to use in the template processing
func setAppConstants(common *v1.CommonConfig) {
	if len(common.Version) == 0 {
		common.Version = constants.CurrentVersion
	} else if common.Version == "74" {
		common.Version = "7.4.1"
	}
	if len(common.AdminUser) == 0 {
		common.AdminUser = constants.DefaultAdminUser
	}
}

// getMinorImageVersion ...
func getMinorImageVersion(productVersion string) string {
	major, minor, _ := MajorMinorPatch(productVersion)
	return fmt.Sprintf("%s%s", major, minor)
}

// MajorMinorPatch ...
func MajorMinorPatch(productVersion string) (major, minor, patch string) {
	version := strings.Split(productVersion, ".")
	for len(version) < 3 {
		version = append(version, "0")
	}
	return version[0], version[1], version[2]
}

// checkVersion ...
func checkVersion(productVersion string) bool {
	versionCheck := make(map[string]bool)
	for _, v := range constants.SupportedVersions {
		versionCheck[v] = true
	}
	if versionCheck[productVersion] {
		return true
	}
	return false
}

// Pint returns a pointer to an integer
func Pint(i int) *int {
	return &i
}

// Pint32 returns a pointer to an integer
func Pint32(i int32) *int32 {
	return &i
}

func GetProduct(env v1.EnvironmentType) (product string) {
	envConstants := constants.EnvironmentConstants[env]
	if envConstants != nil {
		product = envConstants.App.Product
	}
	return
}
