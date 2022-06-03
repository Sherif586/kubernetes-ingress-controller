package kongstate

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/blang/semver/v4"
	"github.com/go-logr/logr"
	"github.com/kong/go-kong/kong"

	"github.com/kong/kubernetes-ingress-controller/v2/internal/annotations"
	"github.com/kong/kubernetes-ingress-controller/v2/internal/store"
	"github.com/kong/kubernetes-ingress-controller/v2/internal/util"
	"github.com/kong/kubernetes-ingress-controller/v2/internal/validation/consumers/credentials"
)

// KongState holds the configuration that should be applied to Kong.
type KongState struct {
	Services       []Service
	Upstreams      []Upstream
	Certificates   []Certificate
	CACertificates []kong.CACertificate
	Plugins        []Plugin
	Consumers      []Consumer
	Version        semver.Version
}

// SanitizedCopy returns a shallow copy with sensitive values redacted best-effort.
func (ks *KongState) SanitizedCopy() *KongState {
	return &KongState{
		Services:  ks.Services,
		Upstreams: ks.Upstreams,
		Certificates: func() (res []Certificate) {
			for _, v := range ks.Certificates {
				res = append(res, *v.SanitizedCopy())
			}
			return
		}(),
		CACertificates: ks.CACertificates,
		Plugins:        ks.Plugins,
		Consumers: func() (res []Consumer) {
			for _, v := range ks.Consumers {
				res = append(res, *v.SanitizedCopy())
			}
			return
		}(),
	}
}

func (ks *KongState) FillConsumersAndCredentials(log logr.Logger, s store.Storer) {
	consumerIndex := make(map[string]Consumer)

	// build consumer index
	for _, consumer := range s.ListKongConsumers() {
		var c Consumer
		if consumer.Username == "" && consumer.CustomID == "" {
			continue
		}
		if consumer.Username != "" {
			c.Username = kong.String(consumer.Username)
		}
		if consumer.CustomID != "" {
			c.CustomID = kong.String(consumer.CustomID)
		}
		c.K8sKongConsumer = *consumer

		log = log.WithValues(
			"kongconsumer_namespace", consumer.Namespace,
			"kongconsumer_name", consumer.Name,
		)
		for _, cred := range consumer.Credentials {
			log = log.WithValues(
				"secret_namespace", consumer.Namespace,
				"secret_name", cred,
			)
			secret, err := s.GetSecret(consumer.Namespace, cred)
			if err != nil {
				log.V(util.ErrorLevel).Info("failed to fetch secret", "error", err)
				continue
			}
			credConfig := map[string]interface{}{}
			for k, v := range secret.Data {
				// TODO populate these based on schema from Kong
				// and remove this workaround
				if k == "redirect_uris" {
					credConfig[k] = strings.Split(string(v), ",")
					continue
				}
				// TODO this is a kongCredType-agnostic mutation that should only apply to Oauth2 credentials.
				// However, the credential-specific code after deals only in interface{}s, and we can't fix individual
				// keys. To handle this properly we'd need to refactor the types used in all following code.
				if k == "hash_secret" {
					boolVal, err := strconv.ParseBool(string(v))
					if err != nil {
						log.V(util.ErrorLevel).Info("failed to parse hash_secret to bool. defaulting to false",
							"error", err)
						credConfig[k] = false
					} else {
						credConfig[k] = boolVal
					}
					continue
				}
				credConfig[k] = string(v)
			}
			credType, ok := credConfig["kongCredType"].(string)
			if !ok {
				err := fmt.Errorf("invalid credType: %v", credType)
				log.V(util.ErrorLevel).Info("failed to provision credential", "error", err)
			}
			if !credentials.SupportedTypes.Has(credType) {
				err := fmt.Errorf("invalid credType: %v", credType)
				log.V(util.ErrorLevel).Info("failed to provision credential", "error", err)
				continue
			}
			if len(credConfig) <= 1 { // 1 key of credType itself
				log.V(util.ErrorLevel).Info("failed to provision credential", "error", "empty secret")
				continue
			}
			err = c.SetCredential(credType, credConfig)
			if err != nil {
				log.V(util.ErrorLevel).Info("failed to provision credential", "error", err)
				continue
			}
		}

		consumerIndex[consumer.Namespace+"/"+consumer.Name] = c
	}

	// populate the consumer in the state
	for _, c := range consumerIndex {
		ks.Consumers = append(ks.Consumers, c)
	}
}

func (ks *KongState) FillOverrides(log logr.Logger, s store.Storer) {
	for i := 0; i < len(ks.Services); i++ {
		// Services
		kongIngress, err := getKongIngressForServices(s, ks.Services[i].K8sServices)
		if err != nil {
			msg := fmt.Sprintf("failed to fetch KongIngress resource for Services %s",
				PrettyPrintServiceList(ks.Services[i].K8sServices))
			log.V(util.ErrorLevel).Info(msg, "error", err)
			continue
		}

		for _, svc := range ks.Services[i].K8sServices {
			ks.Services[i].override(kongIngress, svc.Annotations)
		}

		// Routes
		for j := 0; j < len(ks.Services[i].Routes); j++ {
			kongIngress, err := getKongIngressFromObjectMeta(s, &ks.Services[i].Routes[j].Ingress)
			if err != nil {
				log.V(util.ErrorLevel).Info("failed to fetch KongIngress resource",
					"resource_namespace", ks.Services[i].Routes[j].Ingress.Namespace,
					"resource_name", ks.Services[i].Routes[j].Ingress.Name,
					"error", err)
			}

			ks.Services[i].Routes[j].override(log, kongIngress)
		}
	}

	// Upstreams
	for i := 0; i < len(ks.Upstreams); i++ {
		kongIngress, err := getKongIngressForServices(s, ks.Upstreams[i].Service.K8sServices)
		if err != nil {
			msg := fmt.Sprintf("failed to fetch KongIngress resource for Services %s",
				PrettyPrintServiceList(ks.Upstreams[i].Service.K8sServices))
			log.V(util.ErrorLevel).Info(msg, "error", err)
			continue
		}

		for _, svc := range ks.Upstreams[i].Service.K8sServices {
			ks.Upstreams[i].override(kongIngress, svc.Annotations)
		}
	}
}

func (ks *KongState) getPluginRelations() map[string]util.ForeignRelations {
	// KongPlugin key (KongPlugin's name:namespace) to corresponding associations
	pluginRels := map[string]util.ForeignRelations{}
	addConsumerRelation := func(namespace, pluginName, identifier string) {
		pluginKey := namespace + ":" + pluginName
		relations, ok := pluginRels[pluginKey]
		if !ok {
			relations = util.ForeignRelations{}
		}
		relations.Consumer = append(relations.Consumer, identifier)
		pluginRels[pluginKey] = relations
	}
	addRouteRelation := func(namespace, pluginName, identifier string) {
		pluginKey := namespace + ":" + pluginName
		relations, ok := pluginRels[pluginKey]
		if !ok {
			relations = util.ForeignRelations{}
		}
		relations.Route = append(relations.Route, identifier)
		pluginRels[pluginKey] = relations
	}
	addServiceRelation := func(namespace, pluginName, identifier string) {
		pluginKey := namespace + ":" + pluginName
		relations, ok := pluginRels[pluginKey]
		if !ok {
			relations = util.ForeignRelations{}
		}
		relations.Service = append(relations.Service, identifier)
		pluginRels[pluginKey] = relations
	}

	for i := range ks.Services {
		// service
		for _, svc := range ks.Services[i].K8sServices {
			pluginList := annotations.ExtractKongPluginsFromAnnotations(svc.GetAnnotations())
			for _, pluginName := range pluginList {
				addServiceRelation(svc.Namespace, pluginName, *ks.Services[i].Name)
			}
		}
		// route
		for j := range ks.Services[i].Routes {
			ingress := ks.Services[i].Routes[j].Ingress
			pluginList := annotations.ExtractKongPluginsFromAnnotations(ingress.Annotations)
			for _, pluginName := range pluginList {
				addRouteRelation(ingress.Namespace, pluginName, *ks.Services[i].Routes[j].Name)
			}
		}
	}
	// consumer
	for _, c := range ks.Consumers {
		pluginList := annotations.ExtractKongPluginsFromAnnotations(c.K8sKongConsumer.GetAnnotations())
		for _, pluginName := range pluginList {
			addConsumerRelation(c.K8sKongConsumer.Namespace, pluginName, *c.Username)
		}
	}
	return pluginRels
}

func buildPlugins(log logr.Logger, s store.Storer, pluginRels map[string]util.ForeignRelations) []Plugin {
	var plugins []Plugin

	for pluginIdentifier, relations := range pluginRels {
		identifier := strings.Split(pluginIdentifier, ":")
		namespace, kongPluginName := identifier[0], identifier[1]
		plugin, err := getPlugin(s, namespace, kongPluginName)
		if err != nil {
			log.V(util.ErrorLevel).Info("failed to fetch KongPlugin",
				"kongplugin_namespace", namespace,
				"kongplugin_name", kongPluginName,
			)
			continue
		}

		for _, rel := range relations.GetCombinations() {
			plugin := *plugin.DeepCopy()
			// ID is populated because that is read by decK and in_memory
			// translator too
			if rel.Service != "" {
				plugin.Service = &kong.Service{ID: kong.String(rel.Service)}
			}
			if rel.Route != "" {
				plugin.Route = &kong.Route{ID: kong.String(rel.Route)}
			}
			if rel.Consumer != "" {
				plugin.Consumer = &kong.Consumer{ID: kong.String(rel.Consumer)}
			}
			plugins = append(plugins, Plugin{plugin})
		}
	}

	globalPlugins, err := globalPlugins(log, s)
	if err != nil {
		log.V(util.ErrorLevel).Info("failed to fetch global plugins", "error", err)
	}
	plugins = append(plugins, globalPlugins...)

	return plugins
}

func globalPlugins(log logr.Logger, s store.Storer) ([]Plugin, error) {
	// removed as of 0.10.0
	// only retrieved now to warn users
	globalPlugins, err := s.ListGlobalKongPlugins()
	if err != nil {
		return nil, fmt.Errorf("error listing global KongPlugins: %w", err)
	}
	if len(globalPlugins) > 0 {
		log.V(util.WarnLevel).Info("global KongPlugins found. These are no longer applied and" +
			" must be replaced with KongClusterPlugins." +
			" Please run \"kubectl get kongplugin -l global=true --all-namespaces\" to list existing plugins")
	}
	res := make(map[string]Plugin)
	var duplicates []string // keep track of duplicate
	// TODO respect the oldest CRD
	// Current behavior is to skip creating the plugin but in case
	// of duplicate plugin definitions, we should respect the oldest one
	// This is important since if a user comes in to k8s and creates a new
	// CRD, the user now deleted an older plugin

	globalClusterPlugins, err := s.ListGlobalKongClusterPlugins()
	if err != nil {
		return nil, fmt.Errorf("error listing global KongClusterPlugins: %w", err)
	}
	for i := 0; i < len(globalClusterPlugins); i++ {
		k8sPlugin := *globalClusterPlugins[i]
		pluginName := k8sPlugin.PluginName
		// empty pluginName skip it
		if pluginName == "" {
			log.V(util.ErrorLevel).Info("invalid KongClusterPlugin: empty plugin property",
				"kongclusterplugin_name", k8sPlugin.Name)
			continue
		}
		if _, ok := res[pluginName]; ok {
			msg := fmt.Sprintf("multiple KongPlugin definitions found with"+
				" 'global' label for '%s', the plugin will not be applied", pluginName)
			log.V(util.ErrorLevel).Info(msg)

			duplicates = append(duplicates, pluginName)
			continue
		}
		if plugin, err := kongPluginFromK8SClusterPlugin(s, k8sPlugin); err == nil {
			res[pluginName] = Plugin{
				Plugin: plugin,
			}
		} else {
			log.V(util.ErrorLevel).Info("failed to generate configuration from KongClusterPlugin",
				"kongclusterplugin_name", k8sPlugin.Name,
				"error", err)
		}
	}
	for _, plugin := range duplicates {
		delete(res, plugin)
	}
	var plugins []Plugin
	for _, p := range res {
		plugins = append(plugins, p)
	}
	return plugins, nil
}

func (ks *KongState) FillPlugins(log logr.Logger, s store.Storer) {
	ks.Plugins = buildPlugins(log, s, ks.getPluginRelations())
}
