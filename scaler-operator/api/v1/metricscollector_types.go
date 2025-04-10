// api/v1/metricscollector_types.go
package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MetricsCollectorSpec definisce lo stato desiderato di MetricsCollector
type MetricsCollectorSpec struct {
	PrometheusURL string `json:"prometheusUrl"` // URL di Prometheus
	CSVFilePath   string `json:"csvFilePath"`   // Percorso del file CSV
}

// MetricsCollectorStatus definisce lo stato osservato di MetricsCollector
type MetricsCollectorStatus struct {
	LastRunTime metav1.Time `json:"lastRunTime,omitempty"` // Ultima esecuzione
	LastSavedTime metav1.Time `json:"lastSavedTime"`  // Aggiungi questo campo
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// MetricsCollector è lo schema per l'API dei metricscollector
type MetricsCollector struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MetricsCollectorSpec   `json:"spec,omitempty"`
	Status MetricsCollectorStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// MetricsCollectorList contiene una lista di MetricsCollector
type MetricsCollectorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MetricsCollector `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MetricsCollector{}, &MetricsCollectorList{})
}
