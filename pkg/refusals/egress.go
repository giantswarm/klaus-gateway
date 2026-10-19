package refusals

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// RefusalsMetric is the egress's refusal counter.
const RefusalsMetric = "agentgateway_atenet_egress_refusals_total"

// The labels of RefusalsMetric.
const (
	labelAtespace = "ate_atespace"
	labelActor    = "ate_actor_name"
	labelScheme   = "scheme"
	labelHost     = "host"
	labelPort     = "port"
)

// maxMetricsBytes bounds one pod's metrics page: the egress bounds its
// refusal series to 4096, far below this.
const maxMetricsBytes = 32 << 20

// EgressPods reads RefusalsMetric from the stats port of every ready pod
// behind the egress's Service, found through its EndpointSlices: a counter
// belongs to one pod, and a Service would hand each read to one of them.
type EgressPods struct {
	Client    kubernetes.Interface
	Namespace string
	Service   string
	// Port is the stats port and Path its metrics path.
	Port int
	Path string
	HTTP *http.Client
}

// Refusals implements Source. A pod that does not answer is skipped, so one
// slow pod does not hide the others' refusals; it fails only when no pod
// answered.
func (e *EgressPods) Refusals(ctx context.Context) ([]Series, error) {
	slices, err := e.Client.DiscoveryV1().EndpointSlices(e.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=" + e.Service,
	})
	if err != nil {
		return nil, fmt.Errorf("refusals: list the endpoints of %s/%s: %w", e.Namespace, e.Service, err)
	}
	var out []Series
	var errs []error
	answered := 0
	for _, slice := range slices.Items {
		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			for _, addr := range ep.Addresses {
				series, err := e.read(ctx, addr)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				answered++
				out = append(out, series...)
			}
		}
	}
	if answered == 0 && len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

func (e *EgressPods) read(ctx context.Context, addr string) ([]Series, error) {
	url := "http://" + net.JoinHostPort(addr, strconv.Itoa(e.Port)) + e.Path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4")
	client := e.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refusals: read %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refusals: read %s: %s", url, resp.Status)
	}
	return ParseRefusals(addr, io.LimitReader(resp.Body, maxMetricsBytes))
}

// ParseRefusals reads the RefusalsMetric series of one metrics page in the
// Prometheus text format, attributed to source.
func ParseRefusals(source string, page io.Reader) ([]Series, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(page)
	if err != nil {
		return nil, fmt.Errorf("refusals: parse the metrics of %s: %w", source, err)
	}
	family, ok := families[RefusalsMetric]
	if !ok {
		return nil, nil
	}
	out := make([]Series, 0, len(family.GetMetric()))
	for _, m := range family.GetMetric() {
		labels := labelMap(m)
		port, _ := strconv.Atoi(labels[labelPort])
		out = append(out, Series{
			Source:   source,
			Atespace: labels[labelAtespace],
			Actor:    labels[labelActor],
			Scheme:   labels[labelScheme],
			Host:     labels[labelHost],
			Port:     port,
			Value:    counterValue(m),
		})
	}
	return out, nil
}

func labelMap(m *dto.Metric) map[string]string {
	labels := make(map[string]string, len(m.GetLabel()))
	for _, l := range m.GetLabel() {
		labels[l.GetName()] = l.GetValue()
	}
	return labels
}

// counterValue is the metric's value: a counter's, or an untyped sample's
// for an exporter that writes no TYPE line.
func counterValue(m *dto.Metric) float64 {
	if c := m.GetCounter(); c != nil {
		return c.GetValue()
	}
	return m.GetUntyped().GetValue()
}
