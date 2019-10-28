/*
Copyright 2019 The Knative Authors

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

package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"

	"knative.dev/pkg/configmap"
	"knative.dev/pkg/kmp"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
)

// ConfigValidationController implements the AdmissionController for ConfigMaps
type ConfigValidationController struct {
	// name of the ValidatingWebhookConfiguration
	name string
	// path that the webhook should serve on
	path         string
	constructors map[string]reflect.Value
}

// NewConfigValidationController constructs a ConfigValidationController
func NewConfigValidationController(
	name, path string,
	constructors configmap.Constructors) AdmissionController {
	cfgValidations := &ConfigValidationController{
		name:         name,
		path:         path,
		constructors: make(map[string]reflect.Value),
	}

	for configName, constructor := range constructors {
		cfgValidations.registerConfig(configName, constructor)
	}

	return cfgValidations
}

// Path implements AdmissionController
func (ac *ConfigValidationController) Path() string {
	return ac.path
}

// Admit implements AdmissionController
func (ac *ConfigValidationController) Admit(ctx context.Context, request *admissionv1beta1.AdmissionRequest) *admissionv1beta1.AdmissionResponse {
	logger := logging.FromContext(ctx)
	switch request.Operation {
	case admissionv1beta1.Create, admissionv1beta1.Update:
	default:
		logger.Infof("Unhandled webhook operation, letting it through %v", request.Operation)
		return &admissionv1beta1.AdmissionResponse{Allowed: true}
	}

	if err := ac.validate(ctx, request); err != nil {
		return makeErrorStatus("validation failed: %v", err)
	}

	return &admissionv1beta1.AdmissionResponse{
		Allowed: true,
	}
}

// Register implements AdmissionController
func (ac *ConfigValidationController) Register(ctx context.Context, kubeClient kubernetes.Interface, caCert []byte) error {
	client := kubeClient.AdmissionregistrationV1beta1().ValidatingWebhookConfigurations()
	logger := logging.FromContext(ctx)

	ruleScope := admissionregistrationv1beta1.NamespacedScope
	rules := []admissionregistrationv1beta1.RuleWithOperations{{
		Operations: []admissionregistrationv1beta1.OperationType{
			admissionregistrationv1beta1.Create,
			admissionregistrationv1beta1.Update,
		},
		Rule: admissionregistrationv1beta1.Rule{
			APIGroups:   []string{""},
			APIVersions: []string{"v1"},
			Resources:   []string{"configmaps/*"},
			Scope:       &ruleScope,
		},
	}}

	configuredWebhook, err := client.Get(ac.name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error retrieving webhook: %v", err)
	}

	webhook := configuredWebhook.DeepCopy()

	// Clear out any previous (bad) OwnerReferences.
	// See: https://github.com/knative/serving/issues/5845
	webhook.OwnerReferences = nil

	for i, wh := range webhook.Webhooks {
		if wh.Name != webhook.Name {
			continue
		}
		webhook.Webhooks[i].Rules = rules
		webhook.Webhooks[i].ClientConfig.CABundle = caCert
		if webhook.Webhooks[i].ClientConfig.Service == nil {
			return fmt.Errorf("missing service reference for webhook: %s", wh.Name)
		}
		webhook.Webhooks[i].ClientConfig.Service.Path = ptr.String(ac.path)
	}

	if ok, err := kmp.SafeEqual(configuredWebhook, webhook); err != nil {
		return fmt.Errorf("error diffing webhooks: %v", err)
	} else if !ok {
		logger.Info("Updating webhook")
		if _, err := client.Update(webhook); err != nil {
			return fmt.Errorf("failed to update webhook: %v", err)
		}
	} else {
		logger.Info("Webhook is valid")
	}

	return nil
}

func (ac *ConfigValidationController) validate(ctx context.Context, req *admissionv1beta1.AdmissionRequest) error {
	logger := logging.FromContext(ctx)
	kind := req.Kind
	newBytes := req.Object.Raw

	// Why, oh why are these different types...
	gvk := schema.GroupVersionKind{
		Group:   kind.Group,
		Version: kind.Version,
		Kind:    kind.Kind,
	}

	resourceGVK := corev1.SchemeGroupVersion.WithKind("ConfigMap")
	if gvk != resourceGVK {
		logger.Errorf("Unhandled kind: %v", gvk)
		return fmt.Errorf("unhandled kind: %v", gvk)
	}

	var newObj corev1.ConfigMap
	if len(newBytes) != 0 {
		newDecoder := json.NewDecoder(bytes.NewBuffer(newBytes))
		if err := newDecoder.Decode(&newObj); err != nil {
			return fmt.Errorf("cannot decode incoming new object: %v", err)
		}
	}

	var err error
	if constructor, ok := ac.constructors[newObj.Name]; ok {

		inputs := []reflect.Value{
			reflect.ValueOf(&newObj),
		}

		outputs := constructor.Call(inputs)
		errVal := outputs[1]

		if !errVal.IsNil() {
			err = errVal.Interface().(error)
		}
	}

	return err
}

func (ac *ConfigValidationController) registerConfig(name string, constructor interface{}) {
	if err := configmap.ValidateConstructor(constructor); err != nil {
		panic(err)
	}

	ac.constructors[name] = reflect.ValueOf(constructor)
}
