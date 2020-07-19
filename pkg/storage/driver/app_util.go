/*
Copyright The Helm Authors.

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

package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"kubepack.dev/kubepack/apis"
	"kubepack.dev/kubepack/apis/kubepack/v1alpha1"
	"kubepack.dev/kubepack/pkg/lib"

	"github.com/gabriel-vasile/mimetype"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	rspb "helm.sh/helm/v3/pkg/release"
	helmtime "helm.sh/helm/v3/pkg/time"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	yu "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/application/api/app/v1beta1"
	"sigs.k8s.io/yaml"
)

// newApplicationSecretsObject constructs a kubernetes Application object
// to store a release. Each configmap data entry is the base64
// encoded gzipped string of a release.
//
// The following labels are used within each configmap:
//
//    "modifiedAt"     - timestamp indicating when this configmap was last modified. (set in Update)
//    "createdAt"      - timestamp indicating when this configmap was created. (set in Create)
//    "version"        - version of the release.
//    "status"         - status of the release (see pkg/release/status.go for variants)
//    "owner"          - owner of the configmap, currently "helm".
//    "name"           - name of the release.
//
func newApplicationObject(rls *rspb.Release, lbs labels) (*v1beta1.Application, error) {
	const owner = "helm"

	if lbs == nil {
		lbs.init()
	}

	// apply labels
	lbs.set("name", rls.Name)
	lbs.set("owner", owner)
	lbs.set("status", release.StatusDeployed.String())
	lbs.set("version", strconv.Itoa(rls.Version))

	p := v1alpha1.ApplicationPackage{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.SchemeGroupVersion.String(),
			Kind:       "ApplicationPackage",
		},
		// Bundle: x.Chart.Bundle,
		Chart: v1alpha1.ChartRepoRef{
			Name: rls.Chart.Metadata.Name,
			// URL:     rls.Chart.Metadata.Sources[0],
			Version: rls.Chart.Metadata.Version,
		},
		Channel: v1alpha1.RegularChannel,
	}
	data, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}

	// create and return configmap object
	obj := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rls.Name,
			Namespace: rls.Namespace,
			Labels:    lbs.toMap(),
			Annotations: map[string]string{
				apis.LabelPackage:        string(data),
				"helm.sh/first-deployed": rls.Info.FirstDeployed.UTC().Format(time.RFC3339),
				"helm.sh/last-deployed":  rls.Info.LastDeployed.UTC().Format(time.RFC3339),
			},
		},
		Spec: v1beta1.ApplicationSpec{
			Descriptor: v1beta1.Descriptor{
				Type:        rls.Chart.Metadata.Type,
				Version:     rls.Chart.Metadata.AppVersion,
				Description: rls.Chart.Metadata.Description,
				Owners:      nil, // FIX
				Keywords:    rls.Chart.Metadata.Keywords,
				Links: []v1beta1.Link{
					{
						Description: string(v1alpha1.LinkWebsite),
						URL:         rls.Chart.Metadata.Home,
					},
				},
				Notes: rls.Info.Notes,
			},
			ComponentGroupKinds: nil,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{},
			},
			AddOwnerRef:   true, // TODO
			AssemblyPhase: v1beta1.Pending,
		},
	}
	if rls.Chart.Metadata.Icon != "" {
		var imgType string
		if resp, err := http.Get(rls.Chart.Metadata.Icon); err == nil {
			if mime, err := mimetype.DetectReader(resp.Body); err == nil {
				imgType = mime.String()
			}
			_ = resp.Body.Close()
		}
		obj.Spec.Descriptor.Icons = []v1beta1.ImageSpec{
			{
				Source: rls.Chart.Metadata.Icon,
				// TotalSize: "",
				Type: imgType,
			},
		}
	}
	for _, maintainer := range rls.Chart.Metadata.Maintainers {
		obj.Spec.Descriptor.Maintainers = append(obj.Spec.Descriptor.Maintainers, v1beta1.ContactData{
			Name:  maintainer.Name,
			URL:   maintainer.URL,
			Email: maintainer.Email,
		})
	}

	components := map[metav1.GroupKind]string{}
	var commonLabels map[string]string

	// Hooks ?
	components, commonLabels, err = extractComponents(rls.Manifest, components, commonLabels)
	if err != nil {
		return nil, err
	}

	gvks := make([]metav1.GroupVersionKind, 0, len(components))
	for gk, v := range components {
		gvks = append(gvks, metav1.GroupVersionKind{
			Group:   gk.Group,
			Version: v,
			Kind:    gk.Kind,
		})
	}
	sort.Slice(gvks, func(i, j int) bool {
		if gvks[i].Group == gvks[j].Group {
			return gvks[i].Kind < gvks[j].Kind
		}
		return gvks[i].Group < gvks[j].Group
	})

	gks := make([]metav1.GroupKind, 0, len(components))
	versions := make([]string, 0, len(components))
	for _, gvk := range gvks {
		gks = append(gks, metav1.GroupKind{
			Group: gvk.Group,
			Kind:  gvk.Kind,
		})
		versions = append(versions, gvk.Version)
	}
	obj.Spec.ComponentGroupKinds = gks
	obj.Annotations["helm.sh/component-versions"] = strings.Join(versions, ",")

	if len(commonLabels) > 0 {
		obj.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: commonLabels,
		}
	}

	return obj, nil
}

func mergeSecret(app *v1beta1.Application, s *corev1.Secret) {
	var found bool
	for _, info := range app.Spec.Info {
		if info.Name == s.Name {
			found = true
			break
		}
	}
	if !found {
		app.Spec.Info = append(app.Spec.Info, v1beta1.InfoItem{
			Name: s.Name,
			Type: v1beta1.ReferenceInfoItemType,
			ValueFrom: &v1beta1.InfoItemSource{
				Type: v1beta1.SecretKeyRefInfoItemSourceType,
				SecretKeyRef: &v1beta1.SecretKeySelector{
					ObjectReference: corev1.ObjectReference{
						Namespace: s.Namespace,
						Name:      s.Name,
					},
					Key: "release",
				},
			},
		})
	}
}

func extractComponents(data string, components map[metav1.GroupKind]string, commonLabels map[string]string) (map[metav1.GroupKind]string, map[string]string, error) {
	reader := yu.NewYAMLOrJSONDecoder(strings.NewReader(data), 2048)
	for {
		var obj unstructured.Unstructured
		err := reader.Decode(&obj)
		if err == io.EOF {
			break
		} else if err != nil {
			return components, commonLabels, err
		}
		if obj.IsList() {
			err := obj.EachListItem(func(item runtime.Object) error {
				castItem := item.(*unstructured.Unstructured)

				gv, err := schema.ParseGroupVersion(castItem.GetAPIVersion())
				if err != nil {
					return err
				}
				components[metav1.GroupKind{Group: gv.Group, Kind: castItem.GetKind()}] = gv.Version

				if commonLabels == nil {
					commonLabels = castItem.GetLabels()
				} else {
					for k, v := range castItem.GetLabels() {
						if existing, found := commonLabels[k]; found && existing != v {
							delete(commonLabels, k)
						}
					}
				}
				return nil
			})
			if err != nil {
				return components, commonLabels, err
			}
		} else {
			gv, err := schema.ParseGroupVersion(obj.GetAPIVersion())
			if err != nil {
				return components, commonLabels, err
			}
			components[metav1.GroupKind{Group: gv.Group, Kind: obj.GetKind()}] = gv.Version

			if commonLabels == nil {
				commonLabels = obj.GetLabels()
			} else {
				for k, v := range obj.GetLabels() {
					if existing, found := commonLabels[k]; found && existing != v {
						delete(commonLabels, k)
					}
				}
			}
		}
	}
	return components, commonLabels, nil
}

// decodeRelease decodes the bytes of data into a release
// type. Data must contain a base64 encoded gzipped string of a
// valid release, otherwise an error is returned.
func decodeReleaseFromApp(app *v1beta1.Application, di dynamic.Interface, cl discovery.CachedDiscoveryInterface) (*rspb.Release, error) {
	var rls rspb.Release

	rls.Name = app.Labels["name"]
	rls.Namespace = app.Namespace
	rls.Version, _ = strconv.Atoi(app.Labels["version"])

	var ap v1alpha1.ApplicationPackage
	if data, ok := app.Labels[apis.LabelPackage]; ok {
		err := json.Unmarshal([]byte(data), &ap)
		if err != nil {
			return nil, err
		}
	}
	if ap.Chart.URL != "" &&
		ap.Chart.Name != "" &&
		ap.Chart.Version != "" {
		chrt, err := lib.DefaultRegistry.GetChart(ap.Chart.URL, ap.Chart.Name, ap.Chart.Version)
		if err != nil {
			return nil, err
		}
		rls.Chart = chrt.Chart
	} else {
		rls.Chart = &chart.Chart{
			Metadata: &chart.Metadata{
				Name:    ap.Chart.Name,
				Version: ap.Chart.Version,
			},
		}
	}

	rls.Info = &release.Info{
		Description: app.Spec.Descriptor.Description,
		Status:      release.Status(app.Labels["status"]),
		Notes:       app.Spec.Descriptor.Notes,
	}
	rls.Info.FirstDeployed, _ = helmtime.Parse(time.RFC3339, app.Annotations["helm.sh/first-deployed"])
	rls.Info.LastDeployed, _ = helmtime.Parse(time.RFC3339, app.Annotations["helm.sh/last-deployed"])

	sel, err := metav1.LabelSelectorAsSelector(app.Spec.Selector)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	values := map[string]interface{}{}

	versions := strings.Split(app.Annotations["helm.sh/component-versions"], ",")
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cl)
	for i, gk := range app.Spec.ComponentGroupKinds {
		mapping, err := mapper.RESTMapping(schema.GroupKind{
			Group: gk.Group,
			Kind:  gk.Kind,
		}, versions[i])
		if err != nil {
			return nil, err
		}
		mapping.Resource.Version = versions[i]

		var ri dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			ri = di.Resource(mapping.Resource).Namespace(app.Namespace)
		} else {
			ri = di.Resource(mapping.Resource)
		}

		list, err := ri.List(context.TODO(), metav1.ListOptions{
			LabelSelector: sel.String(),
		})
		if err != nil {
			return nil, err
		}

		err = list.EachListItem(func(obj runtime.Object) error {
			buf.WriteString("\n---\n")
			data, err := yaml.Marshal(obj)
			if err != nil {
				return err
			}
			buf.Write(data)

			u := obj.(*unstructured.Unstructured)

			err = unstructured.SetNestedField(values, u.Object["metadata"], u.GetAPIVersion(), u.GetKind(), "metadata")
			if err != nil {
				return err
			}
			if spec, ok := u.Object["spec"]; ok {
				err = unstructured.SetNestedField(values, spec, u.GetAPIVersion(), u.GetKind(), "spec")
				if err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	rls.Manifest = buf.String()

	if rls.Chart == nil {
		rls.Chart = &chart.Chart{}
	}
	rls.Chart.Values = values
	rls.Config = map[string]interface{}{}

	return &rls, nil
}
