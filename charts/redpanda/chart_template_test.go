package redpanda_test

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	jsoniter "github.com/json-iterator/go"
	"github.com/redpanda-data/helm-charts/charts/redpanda"
	"github.com/redpanda-data/helm-charts/pkg/helm"
	"github.com/redpanda-data/helm-charts/pkg/kube"
	"github.com/redpanda-data/helm-charts/pkg/testutil"
	"github.com/redpanda-data/helm-charts/pkg/valuesutil"
	"github.com/redpanda-data/redpanda/src/go/rpk/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/txtar"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/jsonpath"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

func TestTemplate(t *testing.T) {
	ctx := testutil.Context(t)
	client, err := helm.New(helm.Options{ConfigHome: testutil.TempDir(t)})
	require.NoError(t, err)

	// Chart deps are kept within ./charts as a tgz archive, which is git
	// ignored. Helm dep build will ensure that ./charts is in sync with
	// Chart.lock, which is tracked by git.
	require.NoError(t, client.RepoAdd(ctx, "redpanda", "https://charts.redpanda.com"))
	require.NoError(t, client.DependencyBuild(ctx, "."), "failed to refresh helm dependencies")

	archive, err := txtar.ParseFile("testdata/template-cases.txtar")
	require.NoError(t, err)

	goldens := testutil.NewTxTar(t, "testdata/template-cases.golden.txtar")

	cases := archive.Files
	cases = append(cases, VersionGoldenTestsCases(t)...)
	cases = append(cases, CIGoldenTestCases(t)...)

	for _, tc := range cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			// To make it easy to add tests and assertions on various sets of
			// data, we add markers in YAML comments in the form of:
			// # ASSERT-<NAME> ["OPTIONAL", "PARAMS", "AS", "JSON"]
			// Assertions are run in the order they are specified. A good default
			// if you've got no other options is ASSERT-NO-ERROR.
			assertions := regexp.MustCompile(`(?m)^# (ASSERT-\S+) *(.+)?$`).FindAllSubmatch(tc.Data, -1)
			require.NotEmpty(t, assertions, "no ASSERT- markers found. All cases must have at least 1 marker.")

			var values map[string]any
			require.NoError(t, yaml.Unmarshal(tc.Data, &values))

			out, err := client.Template(ctx, ".", helm.TemplateOptions{
				Name:   "redpanda",
				Values: values,
				Set: []string{
					// Tests utilize some non-deterministic helpers (rng). We don't
					// really care about the stability of their output, so globally
					// disable them.
					"tests.enabled=false",
					// jwtSecret defaults to a random string. Can't have that
					// in snapshot testing so set it to a static value.
					"console.secret.login.jwtSecret=SECRETKEY",
				},
			})

			for _, assertion := range assertions {
				name := string(assertion[1])

				var params []json.RawMessage
				if len(assertion[2]) > 0 {
					require.NoError(t, json.Unmarshal(assertion[2], &params))
				}

				switch name {
				case `ASSERT-NO-ERROR`:
					require.NoError(t, err)

				case `ASSERT-ERROR-CONTAINS`:
					var errFragment string
					require.NoError(t, json.Unmarshal(params[0], &errFragment))
					require.ErrorContains(t, err, errFragment)

				case `ASSERT-GOLDEN`:
					if err == nil {
						goldens.AssertGolden(t, testutil.YAML, fmt.Sprintf("testdata/%s.yaml.golden", t.Name()), out)
					} else {
						// Trailing new lines are added by the txtar format if
						// they're not already present. Add one here otherwise
						// we'll see failures.
						goldens.AssertGolden(t, testutil.Text, fmt.Sprintf("testdata/%s.yaml.golden", t.Name()), []byte(err.Error()+"\n"))
					}

				case `ASSERT-TRUST-STORES`:
					require.NoError(t, err)
					AssertTrustStores(t, out, params)

				case `ASSERT-NO-CERTIFICATES`:
					require.NoError(t, err)
					AssertNoCertficates(t, out)

				case `ASSERT-FIELD-EQUALS`:
					require.NoError(t, err)
					AssertFieldEquals(t, params, out)

				case `ASSERT-VALID-RPK-CONFIGURATION`:
					require.NoError(t, err)
					AssertValidRPKConfiguration(t, out)

				case `ASSERT-STATEFULSET-VOLUME-MOUNTS-VERIFICATION`:
					require.NoError(t, err)
					AssertStatefulSetVolumeMountsVerification(t, out)

				case `ASSERT-STATEFULSET-ALL-VOLUMES-ARE-USED`:
					require.NoError(t, err)
					AssertStatefulsetAllVolumesAreUsed(t, out)

				default:
					t.Fatalf("unknown assertion marker: %q\nFull Line: %s", name, assertion[0])
				}
			}
		})
	}
}

func AssertStatefulSetVolumeMountsVerification(t *testing.T, manifests []byte) {
	objs, err := kube.DecodeYAML(manifests, redpanda.Scheme)
	require.NoError(t, err)

	for _, obj := range objs {
		sts, ok := obj.(*appsv1.StatefulSet)
		if !ok {
			continue
		}

		volumes := map[string]struct{}{}
		for _, v := range sts.Spec.Template.Spec.Containers {
			for _, m := range v.VolumeMounts {
				volumes[m.Name] = struct{}{}
			}
		}

		for _, v := range sts.Spec.Template.Spec.InitContainers {
			for _, m := range v.VolumeMounts {
				volumes[m.Name] = struct{}{}
			}
		}

		for _, v := range sts.Spec.Template.Spec.Volumes {
			delete(volumes, v.Name)
		}
		require.Len(t, volumes, 0)
	}
}

func AssertStatefulsetAllVolumesAreUsed(t *testing.T, manifests []byte) {
	objs, err := kube.DecodeYAML(manifests, redpanda.Scheme)
	require.NoError(t, err)

	for _, obj := range objs {
		sts, ok := obj.(*appsv1.StatefulSet)
		if !ok {
			continue
		}

		volumes := map[string]struct{}{}
		for _, v := range sts.Spec.Template.Spec.Containers {
			for _, m := range v.VolumeMounts {
				volumes[m.Name] = struct{}{}
			}
		}

		for _, v := range sts.Spec.Template.Spec.InitContainers {
			for _, m := range v.VolumeMounts {
				volumes[m.Name] = struct{}{}
			}
		}

		for _, v := range sts.Spec.Template.Spec.Volumes {
			if _, ok := volumes[v.Name]; !ok {
				t.Fatalf("missing volume %s", v.Name)
			}
		}
	}
}

func CIGoldenTestCases(t *testing.T) []txtar.File {
	values, err := os.ReadDir("./ci")
	require.NoError(t, err)

	cases := make([]txtar.File, len(values))
	for i, f := range values {
		data, err := os.ReadFile("./ci/" + f.Name())
		require.NoError(t, err)

		cases[i] = txtar.File{
			Name: f.Name(),
			Data: append([]byte("# ASSERT-NO-ERROR\n# ASSERT-GOLDEN\n# ASSERT-STATEFULSET-ALL-VOLUMES-ARE-USED\n"), data...),
		}
	}
	return cases
}

func VersionGoldenTestsCases(t *testing.T) []txtar.File {
	// A collection of versions that should trigger all the gates guarded by
	// "redpanda-atleast-*" helpers.
	versions := []struct {
		Image  redpanda.PartialImage
		ErrMsg *string
	}{
		{
			Image:  redpanda.PartialImage{Tag: ptr.To(redpanda.ImageTag("v22.1.0"))},
			ErrMsg: ptr.To("no longer supported"),
		},
		{
			Image:  redpanda.PartialImage{Tag: ptr.To(redpanda.ImageTag("v22.2.0"))},
			ErrMsg: ptr.To("does not support TLS on the RPC port. Please upgrade. See technical service bulletin 2023-01."),
		},
		{
			Image:  redpanda.PartialImage{Tag: ptr.To(redpanda.ImageTag("v22.3.0"))},
			ErrMsg: ptr.To("does not support TLS on the RPC port. Please upgrade. See technical service bulletin 2023-01."),
		},
		{
			Image: redpanda.PartialImage{Tag: ptr.To(redpanda.ImageTag("v22.3.14"))},
		},
		{
			Image:  redpanda.PartialImage{Tag: ptr.To(redpanda.ImageTag("v22.4.0"))},
			ErrMsg: ptr.To("does not support TLS on the RPC port. Please upgrade. See technical service bulletin 2023-01."),
		},
		{
			Image:  redpanda.PartialImage{Tag: ptr.To(redpanda.ImageTag("v23.1.1"))},
			ErrMsg: ptr.To("does not support TLS on the RPC port. Please upgrade. See technical service bulletin 2023-01."),
		},
		{
			Image: redpanda.PartialImage{Tag: ptr.To(redpanda.ImageTag("v23.1.2"))},
		},
		{
			Image: redpanda.PartialImage{Tag: ptr.To(redpanda.ImageTag("v23.1.3"))},
		},
		{
			Image: redpanda.PartialImage{Tag: ptr.To(redpanda.ImageTag("v23.2.1"))},
		},
		{
			Image: redpanda.PartialImage{Tag: ptr.To(redpanda.ImageTag("v23.3.0"))},
		},
		{
			Image: redpanda.PartialImage{Tag: ptr.To(redpanda.ImageTag("v24.1.0"))},
		},
		{
			Image: redpanda.PartialImage{Repository: ptr.To("somecustomrepo"), Tag: ptr.To(redpanda.ImageTag("v24.1.0"))},
		},
		{
			Image: redpanda.PartialImage{Repository: ptr.To("somecustomrepo"), Tag: ptr.To(redpanda.ImageTag("v23.2.8"))},
		},
	}

	// A collection of features that are protected by the various above version
	// gates.
	permutations := []redpanda.PartialValues{
		{
			Config: &redpanda.PartialConfig{
				Tunable: redpanda.PartialTunableConfig{
					"log_segment_size_min":  100,
					"log_segment_size_max":  99999,
					"kafka_batch_max_bytes": 7777,
				},
			},
		},
		{
			Enterprise: &redpanda.PartialEnterprise{License: ptr.To("ATOTALLYVALIDLICENSE")},
		},
		{
			RackAwareness: &redpanda.PartialRackAwareness{
				Enabled:        ptr.To(true),
				NodeAnnotation: ptr.To("topology-label"),
			},
		},
	}

	var cases []txtar.File
	for _, version := range versions {
		version := version
		for i, perm := range permutations {
			values, err := valuesutil.UnmarshalInto[redpanda.PartialValues](perm)
			require.NoError(t, err)

			values.Image = &version.Image

			name := fmt.Sprintf("%s-%s-%d", ptr.Deref(version.Image.Repository, "default"), *version.Image.Tag, i)

			header := []byte("# ASSERT-NO-ERROR\n# ASSERT-GOLDEN\n# ASSERT-STATEFULSET-ALL-VOLUMES-ARE-USED\n")
			if version.ErrMsg != nil {
				header = []byte(fmt.Sprintf("# ASSERT-ERROR-CONTAINS [%q]\n# ASSERT-GOLDEN\n", *version.ErrMsg))
			}

			data, err := yaml.Marshal(values)
			require.NoError(t, err)

			cases = append(cases, txtar.File{
				Name: name,
				Data: append(header, data...),
			})
		}
	}
	return cases
}

func AssertTrustStores(t *testing.T, manifests []byte, params []json.RawMessage) {
	var listener string
	var expected map[string]string

	require.NoError(t, json.Unmarshal(params[0], &listener))
	require.NoError(t, json.Unmarshal(params[1], &expected))

	cm, _, err := getConfigMaps(manifests)
	require.NoError(t, err)

	redpandaYAML, err := yaml.YAMLToJSON([]byte(cm.Data["redpanda.yaml"]))
	require.NoError(t, err)

	tlsConfigs := map[string]jsoniter.Any{
		"kafka":           jsoniter.Get(redpandaYAML, "redpanda", "kafka_api_tls"),
		"admin":           jsoniter.Get(redpandaYAML, "redpanda", "admin_api_tls"),
		"http":            jsoniter.Get(redpandaYAML, "pandaproxy", "pandaproxy_api_tls"),
		"schema_registry": jsoniter.Get(redpandaYAML, "schema_registry", "schema_registry_api_tls"),
	}

	actual := map[string]map[string]string{}
	for name, cfg := range tlsConfigs {
		m := map[string]string{}
		for i := 0; i < cfg.Size(); i++ {
			name := cfg.Get(i, "name").ToString()
			truststore := cfg.Get(i, "truststore_file").ToString()
			m[name] = truststore
		}
		actual[name] = m
	}

	assert.Equal(t, expected, actual[listener])
}

func AssertNoCertficates(t *testing.T, manifests []byte) {
	// Assert that no Certificate objects are in the resultant
	// objects when SecretRef is specified AND RequireClientAuth is
	// false.
	objs, err := kube.DecodeYAML(manifests, redpanda.Scheme)
	require.NoError(t, err)

	for _, obj := range objs {
		_, ok := obj.(*certmanagerv1.Certificate)
		// The -root-certificate is always created right now, ignore that
		// one.
		if ok && strings.HasSuffix(obj.GetName(), "-root-certificate") {
			continue
		}
		require.Falsef(t, ok, "Found unexpected Certificate %q", obj.GetName())
	}

	require.NotContains(t, manifests, []byte(certmanagerv1.CertificateKind))
}

func AssertValidRPKConfiguration(t *testing.T, manifests []byte) {
	objs, err := kube.DecodeYAML(manifests, redpanda.Scheme)
	require.NoError(t, err)

	for _, obj := range objs {
		cm, ok := obj.(*corev1.ConfigMap)
		if !(ok && obj.GetName() == "redpanda") {
			continue
		}
		rpCfg, exist := cm.Data["redpanda.yaml"]
		require.True(t, exist, "redpanda.yaml not found")

		var cfg config.RedpandaYaml
		require.NoError(t, yaml.Unmarshal([]byte(rpCfg), &cfg))
	}
}

func AssertFieldEquals(t *testing.T, params []json.RawMessage, manifests []byte) {
	var gvk string
	var key string
	var fieldPath string
	fieldValue := params[3] // No need to unmarshal this one.

	require.NoError(t, json.Unmarshal(params[0], &gvk))
	require.NoError(t, json.Unmarshal(params[1], &key))
	require.NoError(t, json.Unmarshal(params[2], &fieldPath))

	objs, err := kube.DecodeYAML(manifests, redpanda.Scheme)
	require.NoError(t, err)

	for _, obj := range objs {
		kind := obj.GetObjectKind().GroupVersionKind().Kind
		groupVersion := obj.GetObjectKind().GroupVersionKind().GroupVersion().String()

		if groupVersion+"/"+kind != gvk {
			continue
		}

		if obj.GetNamespace()+"/"+obj.GetName() != key {
			continue
		}

		// See https://kubernetes.io/docs/reference/kubectl/jsonpath/
		path := jsonpath.New("").AllowMissingKeys(true)
		require.NoError(t, path.Parse(fieldPath))

		results, err := path.FindResults(obj)
		require.NoError(t, err)

		for _, result := range results {
			actual, err := json.Marshal(result[0].Interface())
			require.NoError(t, err)

			require.JSONEq(t, string(fieldValue), string(actual))
		}

		return
	}

	t.Fatalf("object %q of kind %q not found", gvk, key)
}
