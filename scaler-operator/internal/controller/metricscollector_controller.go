package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"sync"
	"time"
 	"os"
	appsv1 "k8s.io/api/apps/v1" // Import the correct appsv1 package for Deployments, ReplicaSets
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"math"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	_ "github.com/go-sql-driver/mysql"
	metricsv1 "prometheus-operator/api/v1"
)

const (
	RECONCILE_TIME = 60 * time.Minute // Modifica per eseguire il reconcile ogni 60 minuti
)

var mu sync.Mutex
var db *sql.DB
var intervalMinutes = 3
var lastReconcileTime time.Time

// MetricsCollectorReconciler reconciles a MetricsCollector object
type MetricsCollectorReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	AggregatedMetrics map[string]*AggregatedPodData // Aggiungi la mappa qui
	LastSavedTime     *metav1.Time // Nuova variabile per tracciare l'ultimo salvataggio
}

// Definisci una struttura per memorizzare i campioni CPU e Memoria
type MetricsData struct {
	PodName    string
	NodeName   string
	CpuSamples []float64 // I campioni di CPU in millisecondi
	MemSamples []float64 // I campioni di memoria in bytes
}

// AggregatedPodData struttura che contiene i dati aggregati per ogni pod
type AggregatedPodData struct {
	PodNames    string    // Nome/i del pod
	NodeName    string    // Nome del nodo
	CpuAvg      float64   // Media CPU
	MemAvg      float64   // Media Memoria
	CpuVar      float64   // Varianza CPU
	MemVar      float64   // Varianza Memoria
	CpuSamples  []float64 // Campioni CPU
	MemSamples  []float64 // Campioni Memoria
	Owner       string    // Owner del pod (ReplicaSet o Deployment)
	NumReplicas int       // Numero di repliche del pod (ReplicaSet o Deployment)
}

func toPodPointerArray(pods []corev1.Pod) []*corev1.Pod {
	var podPointers []*corev1.Pod
	for i := range pods {
		podPointers = append(podPointers, &pods[i])
	}
	return podPointers
}


func (r *MetricsCollectorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    metricsCollector := &metricsv1.MetricsCollector{}
    if err := r.Get(ctx, req.NamespacedName, metricsCollector); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    lastRunTime := metricsCollector.Status.LastRunTime

    // Esegui ogni minuto, ma salva nel DB solo dopo 60 minuti
    if lastRunTime.IsZero() || time.Since(lastRunTime.Time) < 1*time.Minute {
        logger.Info("Reconcile skipped: Less than 1 minute since last execution")
        return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
    }

	   // Se LastSavedTime è vuota (non è stato mai salvato prima), inizializzala
    if r.LastSavedTime == nil {
        r.LastSavedTime = &metav1.Time{Time: time.Now()}
        logger.Info("Initializing LastSavedTime")
    }

    if time.Since(r.LastSavedTime.Time) >= 60*time.Minute {
        // Salva nel DB e aggiorna LastSavedTime solo dopo 60 minuti
        logger.Info("60 minutes passed, calculating statistics and saving to database")

        // Esegui i calcoli delle medie e varianze per ogni aggregato
        for _, data := range r.AggregatedMetrics {
            avgCPU := calculateMean(data.CpuSamples)
            varCPU := calculateVariance(data.CpuSamples, avgCPU)
            avgMem := calculateMean(data.MemSamples)
            varMem := calculateVariance(data.MemSamples, avgMem)
    
            // Salva i dati nel database
            err := saveToMySQL(data.Owner, data.NodeName, data.PodNames, avgCPU, varCPU, avgMem, varMem, data.NumReplicas, data.CpuSamples, data.MemSamples)
            if err != nil {
                 logger.Error(err, "Error saving metrics to database")
            }
        }

        // Resetta la struttura delle metriche dopo il salvataggio
        r.AggregatedMetrics = make(map[string]*AggregatedPodData)
        logger.Info("Metrics data structure cleared")

        // Aggiorna LastSavedTime solo quando i dati sono stati salvati
        r.LastSavedTime = &metav1.Time{Time: time.Now()}
        logger.Info("LastSavedTime updated")

    }

        prometheusURL := metricsCollector.Spec.PrometheusURL
        nodes, err := r.getNodes(ctx)
        if err != nil {
            logger.Error(err, "Error retrieving nodes")
            return ctrl.Result{}, err
        }

        // Inizializza la mappa di metriche aggregato se è la prima volta
        if r.AggregatedMetrics == nil {
            r.AggregatedMetrics = make(map[string]*AggregatedPodData)
        }

        // Raccolta dei dati dai nodi
        for _, node := range nodes {
            podList, err := r.getPodsForNode(ctx, node.Name)
            if err != nil {
                logger.Error(err, "Error retrieving pods for node", "node", node.Name)
                continue
            }

            var podListPtrs []*corev1.Pod
            for i := range podList {
                podListPtrs = append(podListPtrs, &podList[i])
            }

            err = executePrometheusQuery(ctx, r, prometheusURL, node.Name, r.AggregatedMetrics, podListPtrs)
            if err != nil {
                logger.Error(err, "Error executing Prometheus query", "node", node.Name)
                continue
            }

    // Aggiorna LastRunTime con l'ora corrente
    metricsCollector.Status.LastRunTime = metav1.Now()
    if err := r.Status().Update(ctx, metricsCollector); err != nil {
        logger.Error(err, "Error updating MetricsCollector status with LastRunTime")
        return ctrl.Result{}, err
    }

    // Riprova dopo 1 minuto
    return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
}


func (r *MetricsCollectorReconciler) getPodOwnerAndReplicas(ctx context.Context, podName string, nodeName string, podList []*corev1.Pod) (string, int, error) {
	var foundPod *corev1.Pod
	for _, pod := range podList {
		if pod.Name == podName {
			foundPod = pod
			break
		}
	}

	if foundPod == nil {
		return "", 1, fmt.Errorf("pod %s not found on node %s", podName, nodeName)
	}

	if len(foundPod.OwnerReferences) > 0 {
		for _, owner := range foundPod.OwnerReferences {
			if owner.Kind == "ReplicaSet" {
				// Recupera il ReplicaSet per ottenere il Deployment e il numero di repliche
				rs := &appsv1.ReplicaSet{}
				if err := r.Get(ctx, client.ObjectKey{Name: owner.Name, Namespace: foundPod.Namespace}, rs); err != nil {
					return "", 1, err
				}

				// Controlla se il ReplicaSet è controllato da un Deployment
				if len(rs.OwnerReferences) > 0 {
					for _, rsOwner := range rs.OwnerReferences {
						if rsOwner.Kind == "Deployment" {
							return rsOwner.Name, int(*rs.Spec.Replicas), nil
						}
					}
				}

				// Se non è gestito da un Deployment, ritorna il nome del ReplicaSet e il numero di repliche
				return rs.Name, int(*rs.Spec.Replicas), nil
			}

			if owner.Kind == "Deployment" {
				return owner.Name, 1, nil
			}
		}
	}
	return "", 1, nil
}

// Funzione per calcolare la media dei campioni
func calculateMean(samples []float64) float64 {
	var sum float64
	for _, sample := range samples {
		sum += sample
	}
	return sum / float64(len(samples))
}

// Funzione per calcolare la varianza dei campioni
func calculateVariance(samples []float64, mean float64) float64 {
	var variance float64
	for _, sample := range samples {
		variance += math.Pow(sample-mean, 2)
	}
	return variance / float64(len(samples))
}


// Funzione aggiornata per salvare i dati nel database MySQL
func saveToMySQL(ownerPod, nodeName, podName string, avgCPU, varCPU, avgMem, varMem float64, numReplicas int, cpuSamples []float64, memSamples []float64) error {
    // Serializza i campioni di CPU e memoria in formato JSON
    cpuUsageList, err := json.Marshal(cpuSamples)
    if err != nil {
        fmt.Printf("Error serializing CPU samples: %v\n", err)
        return fmt.Errorf("error serializing CPU samples: %w", err)
    }

    memUsageList, err := json.Marshal(memSamples)
    if err != nil {
        fmt.Printf("Error serializing Memory samples: %v\n", err)
        return fmt.Errorf("error serializing Memory samples: %w", err)
    }

    // Stampa i valori che verranno inseriti nel database per il debug
    fmt.Printf("Saving data to MySQL:\n")
    fmt.Printf("Owner Pod: %s\n", ownerPod)
    fmt.Printf("Node Name: %s\n", nodeName)
    fmt.Printf("Pod Name: %s\n", podName)
    fmt.Printf("Average CPU: %f\n", avgCPU)
    fmt.Printf("CPU Variance: %f\n", varCPU)
    fmt.Printf("Average Memory: %f\n", avgMem)
    fmt.Printf("Memory Variance: %f\n", varMem)
    fmt.Printf("Number of Replicas: %d\n", numReplicas)
    fmt.Printf("CPU Usage List: %s\n", cpuUsageList)
    fmt.Printf("Memory Usage List: %s\n", memUsageList)

    // Prepariamo la query per inserire i dati
    query := `INSERT INTO metrics (timestamp, owner_pod, node_name, pod_name, avg_cpu, var_cpu, avg_mem, var_mem, num_replicas,  cpu_samples , memory_samples)
              VALUES (NOW(), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

    // Eseguiamo la query per inserire i dati nel database
    _, err = db.Exec(query, ownerPod, nodeName, podName, avgCPU, varCPU, avgMem, varMem, numReplicas, string(cpuUsageList), string(memUsageList))
    if err != nil {
        // Se c'è un errore durante l'inserimento, stampiamo l'errore
        fmt.Printf("Error inserting data: %v\n", err)
        return fmt.Errorf("error inserting data into database: %w", err)
    }

    // Se l'inserimento è riuscito, stampiamo una conferma
    fmt.Println("Data successfully inserted into the database.")
    return nil
}

func SetupMySQLConfig() error {
	var err error

	// Legge i parametri dalle variabili d'ambiente
	username := os.Getenv("MYSQL_USER")
	password := os.Getenv("MYSQL_PASSWORD")
	host := os.Getenv("MYSQL_HOST")
	port := os.Getenv("MYSQL_PORT")
	dbName := os.Getenv("MYSQL_DATABASE")

	// Stampa i valori per il debug
	fmt.Printf("MySQL Config:\n")
	fmt.Printf("Username: %s\n", username)
	fmt.Printf("Password: %s\n", password)
	fmt.Printf("Host: %s\n", host)
	fmt.Printf("Port: %s\n", port)
	fmt.Printf("Database: %s\n", dbName)

	// Costruisci la stringa di connessione
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s", username, password, host, port, dbName)
	fmt.Printf("DSN: %s\n", dsn)

	// Connessione al database
	db, err = sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("error connecting to MySQL: %w", err)
	}

	// Verifica la connessione
	if err := db.Ping(); err != nil {
		return fmt.Errorf("error pinging the database: %w", err)
	}

	log.Log.Info("Connected to MySQL database")
	return nil
}

// Function to retrieve nodes from the cluster
func (r *MetricsCollectorReconciler) getNodes(ctx context.Context) ([]corev1.Node, error) {
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		return nil, err
	}

	uniqueNodes := make(map[string]corev1.Node)
	for _, node := range nodeList.Items {
		uniqueNodes[node.Name] = node
	}

	var uniqueNodesList []corev1.Node
	for _, node := range uniqueNodes {
		uniqueNodesList = append(uniqueNodesList, node)
	}

	return uniqueNodesList, nil
}

// Function to retrieve pods for a specific node
func (r *MetricsCollectorReconciler) getPodsForNode(ctx context.Context, nodeName string) ([]corev1.Pod, error) {
	pods, err := r.getPods(ctx)
	if err != nil {
		return nil, err
	}

	var filteredPods []corev1.Pod
	for _, pod := range pods {
		if pod.Spec.NodeName == nodeName {
			filteredPods = append(filteredPods, pod)
		}
	}

	return filteredPods, nil
}

func (r *MetricsCollectorReconciler) getPods(ctx context.Context) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList); err != nil {
		return nil, err
	}
	return podList.Items, nil
}

func float64SliceToInterfaceSlice(input []float64) []interface{} {
	result := make([]interface{}, len(input))
	for i, v := range input {
		result[i] = v
	}
	return result
}

func executePrometheusQuery(ctx context.Context, r *MetricsCollectorReconciler, url, nodeName string, aggregatedMetrics map[string]*AggregatedPodData, podList []*corev1.Pod) error {
	// Query CPU
	cpuURL := getPrometheusQueryURL(nodeName, "cpu")
	fmt.Println("Query CPU:", cpuURL) // Stampa della query CPU
	if err := processPrometheusResponse(cpuURL, aggregatedMetrics, nodeName, true, r, podList); err != nil {
		return err
	}

	// Query Memoria
	memURL := getPrometheusQueryURL(nodeName, "mem")
	fmt.Println("Query Memoria:", memURL) // Stampa della query Memoria
	if err := processPrometheusResponse(memURL, aggregatedMetrics, nodeName, false, r, podList); err != nil {
		return err
	}
	return nil
}


func processPrometheusResponse(queryURL string, metrics map[string]*AggregatedPodData, nodeName string, isCPU bool, r *MetricsCollectorReconciler, podList []*corev1.Pod) error {
	resp, err := http.Get(queryURL)
	if err != nil {
		return fmt.Errorf("errore nella richiesta HTTP per %s: %w", queryURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("query Prometheus fallita (%s): %s", queryURL, resp.Status)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("errore nella lettura della risposta: %w", err)
	}

	type PrometheusMetric struct {
		Metric map[string]string `json:"metric"`
		Value  []interface{}     `json:"value"`
	}
	type PrometheusResponse struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string             `json:"resultType"`
			Result     []PrometheusMetric `json:"result"`
		} `json:"data"`
	}

	var results PrometheusResponse
	if err := json.Unmarshal(body, &results); err != nil {
		return fmt.Errorf("errore nella decodifica della risposta JSON: %w", err)
	}

	// Mappa temporanea per sommare i valori prima di inserirli
	tempSums := make(map[string]float64)

	// Conta i campioni ricevuti per ogni pod
	sampleCounts := make(map[string]int)

	for _, res := range results.Data.Result {
		podName := res.Metric["pod"]
		if podName == "" {
			fmt.Println("Nome pod vuoto, salto questo pod.")
			continue
		}

		if len(res.Value) != 2 {
			return fmt.Errorf("errore: il campo value non contiene 2 elementi come previsto, ma ha %d elementi", len(res.Value))
		}

		valStr, ok := res.Value[1].(string)
		if !ok {
			return fmt.Errorf("errore nel parsing del valore numerico: atteso stringa, ottenuto %T", res.Value[1])
		}

		numVal, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			return fmt.Errorf("errore nel parsing del valore numerico: %w", err)
		}

		// Somma i valori per ogni pod
		tempSums[podName] += numVal
		sampleCounts[podName]++
	}

	// Ora aggiorniamo la struttura metrics con i valori aggregati
	for podName, sum := range tempSums {
		if _, found := metrics[podName]; !found {
			metrics[podName] = &AggregatedPodData{
				PodNames:   podName,
				CpuSamples: []float64{},
				MemSamples: []float64{},
			}

			metrics[podName].NodeName = nodeName
			owner, replicas, err := r.getPodOwnerAndReplicas(context.Background(), podName, nodeName, podList)
			if err != nil {
				return fmt.Errorf("errore nel recuperare l'owner e il numero di repliche del pod: %w", err)
			}

			metrics[podName].Owner = owner
			metrics[podName].NumReplicas = replicas
		}

		// Aggiunge il valore aggregato ai campioni
		if isCPU {
			metrics[podName].CpuSamples = append(metrics[podName].CpuSamples, sum*1000) // CPU in ms
		} else {
			metrics[podName].MemSamples = append(metrics[podName].MemSamples, sum) // Memoria in MB
		}
	}

	return nil
}

func findPodByName(podList []*corev1.Pod, podName string) (*corev1.Pod, error) {
	for _, pod := range podList {
		if pod.Name == podName {
			return pod, nil
		}
	}
	return nil, fmt.Errorf("pod con nome %s non trovato", podName)
}

func getPrometheusQueryURL(nodeName, metricType string) string {
	// Determina la metrica da utilizzare
	metricQuery := ""
	if metricType == "cpu" {
		metricQuery = fmt.Sprintf("rate(container_cpu_usage_seconds_total{kubernetes_io_hostname=\"%s\"}[1m])", nodeName)
	} else if metricType == "mem" {
		metricQuery = fmt.Sprintf("rate(container_memory_usage_bytes{kubernetes_io_hostname=\"%s\"}[1m])", nodeName)
	}

	// Codifica la query
	encodedQuery := encodeQuery(metricQuery)

	// Restituisci l'URL per la query senza intervallo
	return fmt.Sprintf("http://192.168.56.11:30000/api/v1/query?query=%s", encodedQuery)
}

func encodeQuery(str string) string {
	var encoded string
	for i := 0; i < len(str); i++ {
		c := str[i]
		switch c {
		case ' ':
			encoded += "+"
		case '#':
			encoded += "%23"
		case '%':
			encoded += "%25"
		case '&':
			encoded += "%26"
		case '=':
			encoded += "%3D"
		case '?':
			encoded += "%3F"
		case '/':
			encoded += "%2F"
		case '(':
			encoded += "%28"
		case ')':
			encoded += "%29"
		case '{':
			encoded += "%7B"
		case '}':
			encoded += "%7D"
		default:
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
				encoded += string(c)
			} else {
				encoded += "%" + fmt.Sprintf("%02X", c)
			}
		}
	}
	return encoded
}

// SetupWithManager sets up the controller with the Manager.
func (r *MetricsCollectorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Setup MySQL configuration at the start
	if err := SetupMySQLConfig(); err != nil {
	        return fmt.Errorf("failed to set up MySQL: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&metricsv1.MetricsCollector{}).
		Named("metricscollector").
		Complete(r)
}
