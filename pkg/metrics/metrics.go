package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	eventLabel = "event"
	metricsTag = "upgradeoperator"
	nameLabel  = "upgradeconfig_name"
	nodeLabel  = "node_name"

	Namespace = "upgradeoperator"
	Subsystem = "upgrade"

	StateLabel   = "state"
	VersionLabel = "version"

	ScheduledStateValue             = "scheduled"
	StartedStateValue               = "started"
	FinishedStateValue              = "finished"
	ControlPlaneStartedStateValue   = "control_plane_started"
	ControlPlaneCompletedStateValue = "control_plane_completed"
	WorkersStartedStateValue        = "workers_started"
	WorkersCompletedStateValue      = "workers_completed"
)

//go:generate mockgen -destination=mocks/metrics.go -package=mocks github.com/openshift/managed-upgrade-operator/pkg/metrics Metrics
type Metrics interface {
	UpdateMetricValidationFailed(string)
	UpdateMetricValidationSucceeded(string)
	UpdateMetricClusterCheckFailed(string)
	UpdateMetricClusterCheckSucceeded(string)
	ResetMetricClusterCheck(string)
	UpdateMetricScalingFailed(string)
	UpdateMetricScalingSucceeded(string)
	ResetMetricScaling(string)
	UpdateMetricClusterVerificationFailed(string)
	UpdateMetricClusterVerificationSucceeded(string)
	UpdateMetricUpgradeWindowNotBreached(string)
	UpdateMetricUpgradeConfigSynced(string)
	ResetMetricUpgradeConfigSynced(string)
	UpdateMetricUpgradeWindowBreached(string)
	UpdateMetricUpgradeControlPlaneTimeout(string, string)
	ResetMetricUpgradeControlPlaneTimeout(string, string)
	UpdateMetricUpgradeWorkerTimeout(string, string)
	ResetMetricUpgradeWorkerTimeout(string, string)
	UpdateMetricNodeDrainFailed(string)
	ResetMetricNodeDrainFailed(string)
	UpdateMetricNotificationEventSent(string, string, string)
	IsAlertFiring(alert string, checkedNS, ignoredNS []string) (bool, error)
	IsMetricNotificationEventSentSet(upgradeConfigName string, event string, version string) (bool, error)
	IsClusterVersionAtVersion(version string) (bool, error)
	Query(query string) (*AlertResponse, error)
	ResetMetrics()
	ResetAllMetricNodeDrainFailed()
}

//go:generate mockgen -destination=mocks/metrics_builder.go -package=mocks github.com/openshift/managed-upgrade-operator/pkg/metrics MetricsBuilder
type MetricsBuilder interface {
	NewClient(c client.Client) (Metrics, error)
}

func NewBuilder() MetricsBuilder {
	return &metricsBuilder{}
}

type metricsBuilder struct{}

func (mb *metricsBuilder) NewClient(c client.Client) (Metrics, error) {
	promHost, err := getPromHost(c)
	if err != nil {
		return nil, err
	}

	token, err := getPrometheusToken(c)
	if err != nil {
		return nil, err
	}

	return &Counter{
		promHost: *promHost,
		promClient: http.Client{
			Transport: &prometheusRoundTripper{
				token: *token,
			},
		},
	}, nil
}

type prometheusRoundTripper struct {
	token string
}

func (prt *prometheusRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Add("Authorization", "Bearer "+prt.token)
	transport := http.Transport{
		TLSHandshakeTimeout: time.Second * 5,
	}
	return transport.RoundTrip(req)
}

type Counter struct {
	promClient http.Client
	promHost   string
}

var (
	metricValidationFailed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: metricsTag,
		Name:      "upgradeconfig_validation_failed",
		Help:      "Failed to validate the upgrade config",
	}, []string{nameLabel})
	metricClusterCheckFailed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: metricsTag,
		Name:      "cluster_check_failed",
		Help:      "Failed on the cluster check step",
	}, []string{nameLabel})
	metricScalingFailed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: metricsTag,
		Name:      "scaling_failed",
		Help:      "Failed to scale up extra workers",
	}, []string{nameLabel})
	metricClusterVerificationFailed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: metricsTag,
		Name:      "cluster_verification_failed",
		Help:      "Failed on the cluster upgrade verification step",
	}, []string{nameLabel})
	metricUpgradeWindowBreached = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: metricsTag,
		Name:      "upgrade_window_breached",
		Help:      "Failed to commence upgrade during the upgrade window",
	}, []string{nameLabel})
	metricUpgradeConfigSynced = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: metricsTag,
		Name:      "upgradeconfig_synced",
		Help:      "UpgradeConfig has not been synced in time",
	}, []string{nameLabel})
	metricUpgradeControlPlaneTimeout = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: metricsTag,
		Name:      "controlplane_timeout",
		Help:      "Control plane upgrade timeout",
	}, []string{nameLabel, VersionLabel})
	metricUpgradeWorkerTimeout = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: metricsTag,
		Name:      "worker_timeout",
		Help:      "Worker nodes upgrade timeout",
	}, []string{nameLabel, VersionLabel})
	metricNodeDrainFailed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: metricsTag,
		Name:      "node_drain_timeout",
		Help:      "Node cannot be drained successfully in time.",
	}, []string{nodeLabel})
	metricUpgradeNotification = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: metricsTag,
		Name:      "upgrade_notification",
		Help:      "Notification event raised",
	}, []string{nameLabel, eventLabel, VersionLabel})

	metricsList = []*prometheus.GaugeVec{
		metricValidationFailed,
		metricClusterCheckFailed,
		metricScalingFailed,
		metricClusterVerificationFailed,
		metricUpgradeWindowBreached,
		metricUpgradeConfigSynced,
		metricUpgradeControlPlaneTimeout,
		metricUpgradeWorkerTimeout,
		metricNodeDrainFailed,
		metricUpgradeNotification,
	}
)

func init() {
	for _, m := range metricsList {
		metrics.Registry.MustRegister(m)
	}
}

func (c *Counter) UpdateMetricValidationFailed(upgradeConfigName string) {
	metricValidationFailed.With(prometheus.Labels{
		nameLabel: upgradeConfigName}).Set(
		float64(1))
}

func (c *Counter) UpdateMetricValidationSucceeded(upgradeConfigName string) {
	metricValidationFailed.With(prometheus.Labels{
		nameLabel: upgradeConfigName}).Set(
		float64(0))
}

func (c *Counter) UpdateMetricClusterCheckFailed(upgradeConfigName string) {
	metricClusterCheckFailed.With(prometheus.Labels{
		nameLabel: upgradeConfigName}).Set(
		float64(1))
}

func (c *Counter) UpdateMetricClusterCheckSucceeded(upgradeConfigName string) {
	metricClusterCheckFailed.With(prometheus.Labels{
		nameLabel: upgradeConfigName}).Set(
		float64(0))
}

func (c *Counter) ResetMetricClusterCheck(upgradeConfigName string) {
	metricClusterCheckFailed.With(prometheus.Labels{
		nameLabel: upgradeConfigName}).Set(
		float64(0))
}

func (c *Counter) UpdateMetricScalingFailed(upgradeConfigName string) {
	metricScalingFailed.With(prometheus.Labels{
		nameLabel: upgradeConfigName}).Set(
		float64(1))
}

func (c *Counter) UpdateMetricScalingSucceeded(upgradeConfigName string) {
	metricScalingFailed.With(prometheus.Labels{
		nameLabel: upgradeConfigName}).Set(
		float64(0))
}

func (c *Counter) ResetMetricScaling(upgradeConfigName string) {
	metricScalingFailed.With(prometheus.Labels{
		nameLabel: upgradeConfigName}).Set(
		float64(0))
}

func (c *Counter) UpdateMetricUpgradeConfigSynced(name string) {
	metricUpgradeConfigSynced.With(prometheus.Labels{nameLabel: name}).Set(float64(1))
}

func (c *Counter) ResetMetricUpgradeConfigSynced(name string) {
	metricUpgradeConfigSynced.With(prometheus.Labels{nameLabel: name}).Set(float64(0))
}

func (c *Counter) UpdateMetricUpgradeControlPlaneTimeout(upgradeConfigName, version string) {
	metricUpgradeControlPlaneTimeout.With(prometheus.Labels{
		VersionLabel: version,
		nameLabel:    upgradeConfigName}).Set(
		float64(1))
}

func (c *Counter) ResetMetricUpgradeControlPlaneTimeout(upgradeConfigName, version string) {
	metricUpgradeControlPlaneTimeout.With(prometheus.Labels{
		VersionLabel: version,
		nameLabel:    upgradeConfigName}).Set(
		float64(0))
}

func (c *Counter) UpdateMetricUpgradeWorkerTimeout(upgradeConfigName, version string) {
	metricUpgradeWorkerTimeout.With(prometheus.Labels{
		VersionLabel: version,
		nameLabel:    upgradeConfigName}).Set(
		float64(1))
}

func (c *Counter) ResetMetricUpgradeWorkerTimeout(upgradeConfigName, version string) {
	metricUpgradeWorkerTimeout.With(prometheus.Labels{
		VersionLabel: version,
		nameLabel:    upgradeConfigName}).Set(
		float64(0))
}

func (c *Counter) UpdateMetricNodeDrainFailed(nodeName string) {
	metricNodeDrainFailed.With(prometheus.Labels{
		nodeLabel: nodeName}).Set(
		float64(1))
}

func (c *Counter) ResetMetricNodeDrainFailed(nodeName string) {
	metricNodeDrainFailed.With(prometheus.Labels{
		nodeLabel: nodeName}).Set(
		float64(0))
}

func (c *Counter) UpdateMetricClusterVerificationFailed(upgradeConfigName string) {
	metricClusterVerificationFailed.With(prometheus.Labels{
		nameLabel: upgradeConfigName}).Set(
		float64(1))
}

func (c *Counter) UpdateMetricClusterVerificationSucceeded(upgradeConfigName string) {
	metricClusterVerificationFailed.With(prometheus.Labels{
		nameLabel: upgradeConfigName}).Set(
		float64(0))
}

func (c *Counter) UpdateMetricUpgradeWindowNotBreached(upgradeConfigName string) {
	metricUpgradeWindowBreached.With(prometheus.Labels{
		nameLabel: upgradeConfigName}).Set(
		float64(0))
}

func (c *Counter) UpdateMetricUpgradeWindowBreached(upgradeConfigName string) {
	metricUpgradeWindowBreached.With(prometheus.Labels{
		nameLabel: upgradeConfigName}).Set(
		float64(1))
}

func (c *Counter) UpdateMetricNotificationEventSent(upgradeConfigName string, event string, version string) {
	metricUpgradeNotification.With(prometheus.Labels{
		VersionLabel: version,
		eventLabel:   event,
		nameLabel:    upgradeConfigName}).Set(
		float64(1))
}

func (c *Counter) IsMetricNotificationEventSentSet(upgradeConfigName string, event string, version string) (bool, error) {
	cpMetrics, err := c.Query(fmt.Sprintf("%s_upgrade_notification{%s=\"%s\",%s=\"%s\",%s=\"%s\"}", metricsTag, nameLabel, upgradeConfigName, eventLabel, event, VersionLabel, version))
	if err != nil {
		return false, err
	}

	if len(cpMetrics.Data.Result) > 0 {
		return true, nil
	}

	return false, nil
}

func (c *Counter) IsClusterVersionAtVersion(version string) (bool, error) {
	cpMetrics, err := c.Query(fmt.Sprintf("cluster_version{version=\"%s\",type=\"current\"}", version))
	if err != nil {
		return false, err
	}

	if len(cpMetrics.Data.Result) > 0 {
		return true, nil
	}

	return false, nil
}

func (c *Counter) IsAlertFiring(alert string, checkedNS, ignoredNS []string) (bool, error) {
	cpMetrics, err := c.Query(fmt.Sprintf(`ALERTS{alertstate="firing",alertname="%s",namespace=~"^$|%s",namespace!="%s"}`,
		alert, strings.Join(checkedNS, "|"), strings.Join(ignoredNS, "|")))

	if err != nil {
		return false, err
	}

	if len(cpMetrics.Data.Result) > 0 {
		return true, nil
	}
	return false, nil
}

func getPromHost(c client.Client) (*string, error) {
	route := &routev1.Route{}
	err := c.Get(context.TODO(), types.NamespacedName{Namespace: "openshift-monitoring", Name: "prometheus-k8s"}, route)
	if err != nil {
		return nil, err
	}

	return &route.Spec.Host, nil
}

func (c *Counter) Query(query string) (*AlertResponse, error) {
	req, err := http.NewRequest("GET", "https://"+c.promHost+"/api/v1/query", nil)
	if err != nil {
		return nil, fmt.Errorf("Could not query Prometheus: %s", err)
	}

	q := req.URL.Query()
	q.Add("query", query)
	req.URL.RawQuery = q.Encode()
	resp, err := c.promClient.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Error when querying Prometheus: %s", err)
	}

	result := &AlertResponse{}
	err = json.Unmarshal(body, result)
	if err != nil {
		return nil, err
	}

	return result, nil
}

func getPrometheusToken(c client.Client) (*string, error) {
	sa := &corev1.ServiceAccount{}
	err := c.Get(context.TODO(), types.NamespacedName{Namespace: "openshift-monitoring", Name: "prometheus-k8s"}, sa)
	if err != nil {
		return nil, fmt.Errorf("Unable to fetch prometheus-k8s service account: %s", err)
	}

	tokenSecret := ""
	for _, secret := range sa.Secrets {
		if strings.HasPrefix(secret.Name, "prometheus-k8s-token") {
			tokenSecret = secret.Name
		}
	}
	if len(tokenSecret) == 0 {
		return nil, fmt.Errorf("Failed to find token secret for prommetheus-k8s SA")
	}

	secret := &corev1.Secret{}
	err = c.Get(context.TODO(), types.NamespacedName{Namespace: "openshift-monitoring", Name: tokenSecret}, secret)
	if err != nil {
		return nil, fmt.Errorf("Unable to fetch secret %s: %s", tokenSecret, err)
	}

	token := secret.Data[corev1.ServiceAccountTokenKey]
	stringToken := string(token)

	return &stringToken, nil
}

type AlertResponse struct {
	Status string    `json:"status"`
	Data   AlertData `json:"data"`
}

type AlertData struct {
	Result []AlertResult `json:"result"`
}

type AlertResult struct {
	Metric map[string]string `json:"metric"`
	Value  []interface{}     `json:"value"`
}

func (c *Counter) ResetMetrics() {
	for _, m := range metricsList {
		m.Reset()
	}
}

func (c *Counter) ResetAllMetricNodeDrainFailed() {
	metricNodeDrainFailed.Reset()
}
