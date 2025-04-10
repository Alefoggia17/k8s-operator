package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	_ "github.com/go-sql-driver/mysql"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	scalerapi "scaler-operator/api/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type Metric struct {
	Timestamp string  // Timestamp della metrica registrata
	NodeName  string  // Nome del nodo su cui gira il pod
	PodName   string  // Nome completo del pod
	AvgCPU    float64 // Media del consumo CPU
	VarCPU    float64 // Varianza del consumo CPU
	AvgMem    float64 // Media del consumo memoria
	VarMem    float64 // Varianza del consumo memoria
}

const (
	CPU_THRESHOLD  = 80.0
	MEM_THRESHOLD  = 80.0
	RECONCILE_TIME = 5 * time.Minute
)

type ScalerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

var intervalMinutes = 5
var lastReconcileTime time.Time
var db *sql.DB

func (r *ScalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	// Recupera l'oggetto Scaler
	scaler := &scalerapi.Scaler{}
	if err := r.Get(ctx, req.NamespacedName, scaler); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Verifica se sono passati almeno 5 minuti dall'ultima esecuzione
	lastRunTime := scaler.Status.LastRunTime
	if lastRunTime.IsZero() || time.Since(lastRunTime.Time) < 5*time.Minute {
		log.Info("Reconcile skipped: Less than 5 minutes since last execution")
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	prometheusURL := scaler.Spec.PrometheusURL
	// Chiama la funzione resetAllLocks per impostare tutti i lock su "free"
	err := r.resetAllLocks(ctx, req.Namespace)
	if err != nil {

		log.Error(err, "Errore nel reset di tutti i lock")
		return ctrl.Result{}, err
	}
	// Ottieni i nodi
	nodes, err := r.getNodes(ctx)
	if err != nil {
		log.Error(err, "Errore nel recupero dei nodi")
		return ctrl.Result{}, err
	}

	// Itera attraverso i nodi
	for _, node := range nodes {
		pods, err := r.getPodsForNode(ctx, node.Name)
		if err != nil {
			log.Error(err, "Errore nel recupero dei pod per il nodo", "node", node.Name)
			continue
		}

		// Recupera la capacità totale di CPU e memoria del nodo
		nodeCPUCapacity, nodeMemCapacity := r.getNodeResourceCapacity(&node)

		// Itera attraverso i pod
		for _, pod := range pods {
			
			schedulableNodes, err := r.findSchedulableNodesForPod(ctx, pod, log)
			if err != nil {
				log.Error(err, "Errore nel trovare i nodi schedulabili", "pod", pod.Name)
				continue
			}

			// Log dei nodi schedulabili trovati per il pod
			log.Info("Nodi schedulabili trovati per il pod", "pod", pod.Name, "nodes", fmt.Sprintf("%v", schedulableNodes))
			ownerName, ownerKind := getPodOwner(&pod, log)
			// Controlla se il pod ha un'annotazione di timestamp
			if timestampStr, exists := pod.Annotations["scaler-operator/timestamp"]; exists {
				createdTime, err := time.Parse(time.RFC3339, timestampStr)
				if err == nil && time.Since(createdTime) < 5*time.Minute {
					log.Info("Pod nuovo, salto il monitoraggio per ora", "pod", pod.Name)
					continue
				}
			}

			avgCPU, avgMem, err := queryPrometheusStatistics(prometheusURL, pod.Namespace, pod.Name, node.Name)
			if err != nil {
				log.Error(err, "Errore nella query a Prometheus", "pod", pod.Name)
				continue
			}

			// Converti i valori di CPU e memoria in percentuale
			avgCPUPercent := (avgCPU / (nodeCPUCapacity * 1000)) * 100
			avgMemPercent := (avgMem / (nodeMemCapacity / (1024 * 1024))) * 100

			// Log delle percentuali
			log.Info("Percentuali delle risorse", "CPU %", avgCPUPercent, "Memoria %", avgMemPercent)

			// Recupera il numero attuale di repliche
			currentReplicas := getCurrentReplicas(ctx, r, pod.Namespace, ownerName, ownerKind, log)
			// Recupera le risorse libere del nodo

			freeCPU, freeMem, err := r.queryNodeFreeResources(prometheusURL, &node, ctx)
			if err != nil {
				log.Error(err, "Errore nel recupero delle risorse libere del nodo", "node", node.Name)
				continue
			}

			targetNode, err := r.findTargetNodeWithMoreResources(ctx, prometheusURL, freeCPU, freeMem, node.Name, log)
			if err != nil {
				log.Error(err, "Errore nel trovare il nodo di destinazione", "pod", pod.Name)
				continue
			}
			if targetNode != nil && avgCPUPercent > CPU_THRESHOLD || avgMemPercent > MEM_THRESHOLD {
				isTargetNodeSchedulable := false
				for _, schedulableNode := range schedulableNodes {
					parts := strings.Split(schedulableNode, ",")
					nodeNamePart := parts[0]
					nodeName := strings.TrimPrefix(nodeNamePart, "Node Name: ")
					if nodeName == targetNode.Name {
						isTargetNodeSchedulable = true
						break
					}
				}
				// Se il targetNode è nella lista dei nodi schedulabili, sposta il pod
				if isTargetNodeSchedulable {
					err := r.relocatePodToNode(ctx, pod, targetNode, log)
					if err != nil {
						log.Error(err, "Errore nel spostare il pod", "pod", pod.Name)
					}
				} else {
					log.Info("Il nodo di destinazione non è schedulabile", "pod", pod.Name, "targetNode", targetNode.Name)
				}
			}
			// Recupera il numero attuale di repliche per il pod
			initialReplicas := getCurrentReplicas(ctx, r, pod.Namespace, ownerName, ownerKind, log)
			// Controlla se l'annotazione initialReplicas è presente
			if initialReplicasStr, exists := pod.Annotations["scaler-operator/initialReplicas"]; exists {
				parsedReplicas, err := strconv.Atoi(initialReplicasStr)
				if err != nil {
					log.Error(err, "Errore nella conversione di initialReplicas", "pod", pod.Name)
				} else {
					initialReplicas = int32(parsedReplicas)
				}
			}
			// Se le repliche attuali sono maggiori di quelle iniziali e le risorse sono sotto la soglia, riduci le repliche
			if currentReplicas > initialReplicas && avgCPUPercent < CPU_THRESHOLD && avgMemPercent < MEM_THRESHOLD {
				if len(schedulableNodes) > 0 {
					err := r.scalePod(ctx, &pod, -1) // Decrementa il numero di repliche
					if err != nil {
						log.Error(err, "Errore nel decrementare le repliche", "pod", pod.Name)
					} else {
						log.Info("Repliche ridotte con successo", "pod", pod.Name)
					}
				} else {
					log.Info("Nessun nodo schedulabile trovato per il pod, non posso ridurre le repliche", "pod", pod.Name)
				}
			}
			// Se la soglia viene superata, scala il pod
			if avgCPUPercent > CPU_THRESHOLD || avgMemPercent > MEM_THRESHOLD {
				// Prima di scalare il pod, verifica che ci siano nodi schedulabili
				if len(schedulableNodes) > 0 {
					err := r.scalePod(ctx, &pod, 1) // Incrementa il numero di repliche
					if err != nil {
						log.Error(err, "Errore durante la creazione del nuovo pod", "pod", pod.Name)
					} else {
						log.Info("Nuovo pod creato con successo", "pod", pod.Name)
					}
				} else {
					log.Info("Nessun nodo schedulabile trovato per il pod, non posso aumentare le repliche", "pod", pod.Name)
				}
			}

		}
	}
	// Aggiorna il timestamp dell'ultima esecuzione
	scaler.Status.LastRunTime = metav1.Now()
	if err := r.Status().Update(ctx, scaler); err != nil {
		log.Error(err, "Failed to update Scaler status")
		return ctrl.Result{}, err
	}

	// Riavvia il reconciler con il nuovo intervallo
	return ctrl.Result{RequeueAfter: RECONCILE_TIME}, nil

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

func getDataFromYesterdayByNodeAndPod(nodeName, podName string) ([]Metric, error) {
	fmt.Printf("Retrieving data for yesterday from MySQL for node: %s and pod: %s\n", nodeName, podName)
	// Estrai il prefisso del pod (prime due parti del nome)
	parts := strings.Split(podName, "-")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid pod name format: %s", podName)
	}
	prefix := parts[0] + "-" + parts[1] + "-%" // Costruiamo il pattern per il LIKE
	// Query per estrarre i dati di tutti i pod che hanno lo stesso prefisso
	query := `SELECT timestamp, node_name, pod_name, avg_cpu, var_cpu, avg_mem, var_mem
              FROM metrics
              WHERE node_name = ? 
              AND pod_name LIKE ?
              AND DATE(timestamp) = CURDATE() - INTERVAL 1 DAY`

	rows, err := db.Query(query, nodeName, prefix)
	if err != nil {
		fmt.Printf("Error querying data: %v\n", err)
		return nil, fmt.Errorf("error querying data from database: %w", err)
	}
	defer rows.Close()
	var metrics []Metric
	for rows.Next() {
		var metric Metric
		if err := rows.Scan(&metric.Timestamp, &metric.NodeName, &metric.PodName, &metric.AvgCPU, &metric.VarCPU, &metric.AvgMem, &metric.VarMem); err != nil {
			fmt.Printf("Error scanning row: %v\n", err)
			return nil, fmt.Errorf("error scanning row: %w", err)
		}
		metrics = append(metrics, metric)
	}
	if err := rows.Err(); err != nil {
		fmt.Printf("Error iterating rows: %v\n", err)
		return nil, fmt.Errorf("error iterating rows: %w", err)
	}
	if len(metrics) == 0 {
		fmt.Println("No matching data found for yesterday.")
	} else {
		fmt.Println("Data retrieved successfully.")
	}
	return metrics, nil
}

func (r *ScalerReconciler) updateController(controller client.Object, nodeAffinity *corev1.NodeAffinity, targetNode string, ctx context.Context) error {
	log := log.FromContext(ctx)
	switch c := controller.(type) {
	case *appsv1.Deployment:
		log.Info("Processing Deployment...")
		c.Spec.Template.Spec.Affinity = &corev1.Affinity{
			NodeAffinity: nodeAffinity,
		}

		if err := r.Update(ctx, c); err != nil {
			return fmt.Errorf("errore nell'aggiornare il Deployment: %w", err)
		}
		return nil
	case *appsv1.ReplicaSet:
		log.Info("Processing ReplicaSet...")
		deploymentName, found := getDeploymentFromReplicaSet(ctx, r, c.Namespace, c.Name, log)
		if found {
			
			var deployment appsv1.Deployment
			if err := r.Get(ctx, client.ObjectKey{Name: deploymentName, Namespace: c.Namespace}, &deployment); err != nil {
				log.Error(err, "Errore nel recupero del Deployment", "deployment", deploymentName)
				return fmt.Errorf("errore nel recuperare il Deployment associato: %w", err)
			}
			log.Info("Il ReplicaSet è gestito da un Deployment, aggiornando il Deployment", "deployment", deploymentName)	
			deployment.Spec.Template.Spec.Affinity = &corev1.Affinity{
				NodeAffinity: nodeAffinity,
			}
			if err := r.Update(ctx, &deployment); err != nil {
				return fmt.Errorf("errore nell'aggiornare il Deployment associato: %w", err)
			}
			return nil
		}
		log.Info("ReplicaSet non gestito da un Deployment, aggiornando direttamente il ReplicaSet")
		c.Spec.Template.Spec.Affinity = &corev1.Affinity{
			NodeAffinity: nodeAffinity,
		}
		if err := r.Update(ctx, c); err != nil {
			return fmt.Errorf("errore nell'aggiornare il ReplicaSet esistente: %w", err)
		}
		return nil

	case *appsv1.StatefulSet:
		log.Info("Processing StatefulSet...")

		// Update the existing StatefulSet
		c.Spec.Template.Spec.Affinity = &corev1.Affinity{
			NodeAffinity: nodeAffinity,
		}

		// Update the StatefulSet in the cluster
		if err := r.Update(ctx, c); err != nil {
			return fmt.Errorf("errore nell'aggiornare lo StatefulSet esistente: %w", err)
		}

		return nil

	default:
		return fmt.Errorf("controller di tipo non supportato: %T", c)
	}
}

func (r *ScalerReconciler) relocatePodToNode(ctx context.Context, pod corev1.Pod, targetNode *corev1.Node, log logr.Logger) error {
	lock, err := r.acquireLock(ctx, pod.Namespace, pod.Name)
	if err != nil {
		log.Error(err, "Errore nell'acquisizione del lock per il pod", "pod", pod.Name)
		return err
	}
	if lock == nil {
		log.Info("Lock non acquisito, saltando la relocation", "pod", pod.Name)
		return nil
	}
	ownerName, ownerKind := getPodOwner(&pod, log)
	nodeAffinity := &corev1.NodeAffinity{
                        RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
                                NodeSelectorTerms: []corev1.NodeSelectorTerm{
                                        {
                                                MatchExpressions: []corev1.NodeSelectorRequirement{
                                                        {
                                                                Key:      "kubernetes.io/hostname",
                                                                Operator: "In",
                                                                Values:   []string{targetNode.Name},
                                                        },
                                                },
                                        },
                                },
                        },
                }

	// Caso in cui il pod non è gestito da un controller
	if ownerKind == "" {
		log.Info("Pod standalone, creando un clone prima di eliminarlo", "pod", pod.Name)

		newPod := pod.DeepCopy()
		newPod.ResourceVersion = ""
		newPod.Spec.Affinity = &corev1.Affinity{
			NodeAffinity: nodeAffinity,
		}
		// Crea il nuovo pod
		if err := r.Create(ctx, newPod); err != nil {
			return fmt.Errorf("errore nella creazione del nuovo pod: %w", err)
		}
		// Attendi che il nuovo pod sia Running
		if err := r.waitForPodRunning(ctx, newPod.Name, newPod.Namespace, log); err != nil {
			return fmt.Errorf("errore nell'attendere che il nuovo pod sia Running: %w", err)
		}
		// Elimina il vecchio pod
		if err := r.Delete(ctx, &pod); err != nil {
			return fmt.Errorf("errore nella cancellazione del pod esistente: %w", err)
		}
		// Rilascia il lock una volta completato il processo
		err = r.releaseLock(ctx, pod.Namespace, pod.Name)
		if err != nil {
			log.Error(err, "Errore nel rilascio del lock per il pod", "pod", pod.Name)
		}
		log.Info("Pod standalone spostato con successo", "pod", pod.Name, "targetNode", targetNode.Name)
		return nil
	}else {

		// Caso in cui il pod è gestito da un controller (Deployment, ReplicaSet, StatefulSet)
		log.Info("Pod gestito da un controller, aggiornando il controller", "ownerKind", ownerKind, "ownerName", ownerName)
		// Recupera il controller esistente (Deployment, ReplicaSet o StatefulSet)
		var controller client.Object
		switch ownerKind {
		case "Deployment":
			controller = &appsv1.Deployment{}
		case "ReplicaSet":
			controller = &appsv1.ReplicaSet{}
		case "StatefulSet":
			controller = &appsv1.StatefulSet{}
		default:
			return fmt.Errorf("tipo di controller non supportato: %s", ownerKind)
		}
		if err := r.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: ownerName}, controller); err != nil {
			return fmt.Errorf("errore nel recuperare il controller %s: %w", ownerName, err)
		}
		// aggiorno  controller con la nuova affinity
		err = r.updateController(controller, nodeAffinity, targetNode.Name, ctx)
		if err != nil {
			return fmt.Errorf("errore aggiornamneto controller controller: %w", err)
		}
	}
	// Rilascia il lock una volta completato il processo
	err = r.releaseLock(ctx, pod.Namespace, pod.Name)
	if err != nil {
		log.Error(err, "Errore nel rilascio del lock per il pod", "pod", pod.Name)
	}

	return nil
}

// waitForPodRunning attende che il pod specificato sia nello stato "Running" prima di procedere
func (r *ScalerReconciler) waitForPodRunning(ctx context.Context, podName, namespace string, log logr.Logger) error {
	log.Info("Attendere che il pod sia in stato Running", "pod", podName, "namespace", namespace)

	// Impostiamo un timeout per evitare un ciclo infinito
	timeout := time.After(5 * time.Minute)     // Timeout di 5 minuti
	ticker := time.NewTicker(10 * time.Second) // Controlla ogni 10 secondi

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout raggiunto: il pod %s non è entrato in stato Running", podName)
		case <-ticker.C:
			// Recuperiamo lo stato del pod
			var pod corev1.Pod
			err := r.Get(ctx, client.ObjectKey{Name: podName, Namespace: namespace}, &pod)
			if err != nil {
				log.Error(err, "Errore nel recupero dello stato del pod", "pod", podName)
				return err
			}

			// Verifica se il pod è nello stato Running
			if pod.Status.Phase == corev1.PodRunning {
				log.Info("Il pod è in stato Running", "pod", podName)
				return nil
			}
		}
	}
}

func (r *ScalerReconciler) getNodeResourceCapacity(node *corev1.Node) (float64, float64) {
    // Recupera le risorse allocabili del nodo
    nodeCPUAllocatable := node.Status.Allocatable["cpu"]
    nodeMemAllocatable := node.Status.Allocatable["memory"]

    // Converti la CPU in secondi (CPU-seconds) per 300 secondi (5 minuti)
    nodeCPUMilli := nodeCPUAllocatable.MilliValue()   // CPU in milliCPU
    nodeMemBytes := nodeMemAllocatable.Value()        // Memoria in bytes

    nodeCPUSeconds := float64(nodeCPUMilli) * 300 / 1000 // Converti milliCPU in secondi

    return nodeCPUSeconds, float64(nodeMemBytes)
}

func (r *ScalerReconciler) queryNodeFreeResources(prometheusURL string, node *corev1.Node, ctx context.Context) (float64, float64, error) {
    // Ottieni la capacità del nodo (CPU in secondi e memoria in bytes)
    nodeCPUSec, nodeMemBytes := r.getNodeResourceCapacity(node)
    // Query Prometheus per ottenere l'uso della CPU e della memoria
    cpuUsedQuery := fmt.Sprintf(`avg(rate(container_cpu_usage_seconds_total{kubernetes_io_hostname="%s"}[5m]))`, node.Name)
    memUsedQuery := fmt.Sprintf(`avg_over_time(container_memory_usage_bytes{kubernetes_io_hostname="%s"}[5m])`, node.Name)
    cpuUsedData, err := executePrometheusQuery(prometheusURL, cpuUsedQuery)
    if err != nil {
        return 0, 0, fmt.Errorf("errore nella query per l'uso della CPU: %w", err)
    }
    usedCPU := calculateLastValue(float64SliceToInterfaceSlice(cpuUsedData)) // Uso CPU in secondi

    memUsedData, err := executePrometheusQuery(prometheusURL, memUsedQuery)
    if err != nil {
        return 0, 0, fmt.Errorf("errore nella query per l'uso della memoria: %w", err)
    }
    usedMem := calculateLastValue(float64SliceToInterfaceSlice(memUsedData)) // Uso Memoria in bytes

    // Calcoliamo le risorse libere
    freeCPU := nodeCPUSec - usedCPU       // CPU libera in secondi
    freeMem := nodeMemBytes - usedMem     // Memoria libera in bytes
    return freeCPU, freeMem, nil 
}

// Funzione per trovare il nodo con più risorse libere rispetto al nodo attuale
func (r *ScalerReconciler) findTargetNodeWithMoreResources(
	ctx context.Context,
	prometheusURL string,
	freeCPU, freeMem float64,
	currentNodeName string, // Aggiungi il nome del nodo attuale
	log logr.Logger) (*corev1.Node, error) {

	// Recupera la lista dei nodi
	nodes, err := r.getNodes(ctx)
	if err != nil {
		log.Error(err, "Errore nel recupero dei nodi")
		return nil, err
	}

	// Inizializza il nodo di destinazione a nil
	var targetNode *corev1.Node

	// Itera attraverso tutti i nodi disponibili
	for _, node := range nodes {
		// Salta il nodo attuale, non possiamo spostare il pod sullo stesso nodo
		if node.Name == currentNodeName {
			continue
		}

		// Escludi i nodi che fanno parte del Control Plane
		if _, isControlPlane := node.Labels["node-role.kubernetes.io/control-plane"]; isControlPlane {
			log.Info("Escludo nodo Control Plane", "node", node.Name)
			continue
		}

		nodeCPUFree, nodeMemFree, err := r.queryNodeFreeResources(prometheusURL, &node, ctx)
		if err != nil {
			log.Error(err, "Errore nel recupero delle risorse libere del nodo", "node", node.Name)
			continue
		}

		log.Info("Risorse libere del nodo", "node", node.Name, "CPU libera", nodeCPUFree, "Memoria libera", nodeMemFree)
		if nodeCPUFree > freeCPU && nodeMemFree > freeMem {

			// Se non è stato ancora trovato un nodo di destinazione, o se questo nodo ha più risorse libere
			if targetNode == nil || (nodeCPUFree > freeCPU && nodeMemFree > freeMem) {
				log.Info(" Nodo con più risorse libere trovato", "nodeCPUFree", nodeCPUFree, "freeCPU", freeCPU, "nodeMemFree", nodeMemFree, "freeMem", freeMem)
				// Aggiorna il nodo target con questo nodo
				targetNode = &node
			}

		}
	}

	// Se un nodo con risorse libere superiori è stato trovato, restituiscilo, altrimenti restituisci nil
	if targetNode != nil {
		log.Info("Nodo selezionato per lo spostamento", "targetNode", targetNode.Name)
	} else {
		log.Info("Nessun nodo con risorse libere superiori trovato")
	}

	return targetNode, nil
}

const lockRetryInterval = 1 * time.Second // Intervallo di attesa tra i tentativi di acquisizione del lock

func (r *ScalerReconciler) acquireLock(ctx context.Context, podNamespace string, ownerName string) (*corev1.ConfigMap, error) {
	log := log.FromContext(ctx)
	lockName := "lock-" + ownerName
	lockConfigMap := &corev1.ConfigMap{}
	timeout := time.After(30 * time.Second)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			log.Info("Timeout acquisizione lock, rilascio forzato", "lockName", lockName)
			r.releaseLock(ctx, podNamespace, ownerName)
		case <-ticker.C:
			err := r.Get(ctx, types.NamespacedName{Name: lockName, Namespace: podNamespace}, lockConfigMap)
			if err == nil {
				log.Info("Lock trovato, in attesa del rilascio", "lockName", lockName)
				continue
			}

			// Se il lock non esiste, lo creiamo
			lockConfigMap = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      lockName,
					Namespace: podNamespace,
				},
				Data: map[string]string{
					"locked": "true",
				},
			}

			err = r.Create(ctx, lockConfigMap)
			if err != nil {
				log.Error(err, "Errore nella creazione della ConfigMap di lock", "lockName", lockName)
				return nil, err
			}

			log.Info("Lock acquisito", "lockName", lockName)
			return lockConfigMap, nil
		}
	}
}

func (r *ScalerReconciler) releaseLock(ctx context.Context, podNamespace string, ownerName string) error {
	log := log.FromContext(ctx)
	lockName := "lock-" + ownerName
	lockConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lockName,
			Namespace: podNamespace,
		},
	}

	err := r.Delete(ctx, lockConfigMap)
	if err != nil {
		log.Error(err, "Errore nella rimozione della ConfigMap di lock", "lockName", lockName)
		return err
	}

	log.Info("Lock rilasciato", "lockName", lockName)
	return nil
}

// resetAllLocks imposta tutti i lock esistenti su "free" all'inizio di ogni riconciliazione
func (r *ScalerReconciler) resetAllLocks(ctx context.Context, namespace string) error {
	log := log.FromContext(ctx)

	configMapList := &corev1.ConfigMapList{}
	err := r.List(ctx, configMapList, client.InNamespace(namespace))
	if err != nil {
		log.Error(err, "Errore nel recuperare la lista delle ConfigMap")
		return err
	}

	for _, cm := range configMapList.Items {
		if _, exists := cm.Data["locked"]; exists {
			cm.Data["locked"] = "free"
			err := r.Update(ctx, &cm)
			if err != nil {
				log.Error(err, "Errore nell'aggiornare il lock a free", "configMap", cm.Name)
			} else {
				log.Info("Lock resettato a free", "configMap", cm.Name)
			}
		}
	}

	return nil
}

// getCurrentReplicas recupera il numero attuale di repliche per un Deployment, ReplicaSet o StatefulSet
func getCurrentReplicas(ctx context.Context, r *ScalerReconciler, namespace, ownerName, ownerKind string, logger logr.Logger) int32 {
	switch ownerKind {
	case "Deployment":
		var deployment appsv1.Deployment
		if err := r.Get(ctx, client.ObjectKey{Name: ownerName, Namespace: namespace}, &deployment); err != nil {
			logger.Error(err, "Errore nel recupero del Deployment", "ownerName", ownerName)
			return 0 // Restituisci 0 in caso di errore
		}
		logger.Info("Recuperato numero repliche per Deployment", "ownerName", ownerName, "replicas", *deployment.Spec.Replicas)
		return *deployment.Spec.Replicas

	case "ReplicaSet":
		var replicaSet appsv1.ReplicaSet
		if err := r.Get(ctx, client.ObjectKey{Name: ownerName, Namespace: namespace}, &replicaSet); err != nil {
			logger.Error(err, "Errore nel recupero del ReplicaSet", "ownerName", ownerName)
			return 0 // Restituisci 0 in caso di errore
		}
		logger.Info("Recuperato numero repliche per ReplicaSet", "ownerName", ownerName, "replicas", *replicaSet.Spec.Replicas)
		return *replicaSet.Spec.Replicas

	case "StatefulSet":
		var statefulSet appsv1.StatefulSet
		if err := r.Get(ctx, client.ObjectKey{Name: ownerName, Namespace: namespace}, &statefulSet); err != nil {
			logger.Error(err, "Errore nel recupero dello StatefulSet", "ownerName", ownerName)
			return 0 // Restituisci 0 in caso di errore
		}
		logger.Info("Recuperato numero repliche per StatefulSet", "ownerName", ownerName, "replicas", *statefulSet.Spec.Replicas)
		return *statefulSet.Spec.Replicas

	default:
		logger.Info("Il tipo di owner non supporta repliche", "ownerKind", ownerKind)
		return 0 // Restituisci 0 se il tipo di owner non è supportato
	}
}


func (r *ScalerReconciler) getNodes(ctx context.Context) ([]corev1.Node, error) {
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		return nil, err
	}
	return nodeList.Items, nil
}

func (r *ScalerReconciler) getPodsForNode(ctx context.Context, nodeName string) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.MatchingFields{"spec.nodeName": nodeName}); err != nil {
		return nil, err
	}
	// Filtro per escludere i pod nel namespace "kube-system"
	var filteredPods []corev1.Pod
	for _, pod := range podList.Items {
		if pod.Namespace == "kube-system" {
			continue // Ignora i pod nel namespace "kube-system"
		}
		filteredPods = append(filteredPods, pod)
	}

	return filteredPods, nil
}
// Funzione per eseguire l'encoding della query
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

func executePrometheusQuery(url, query string) ([]float64, error) {
	// Usa la funzione `encodeQuery` separata per eseguire l'encoding
	encodedQuery := encodeQuery(query)

	// Crea l'URL completo per la query
	fullURL := fmt.Sprintf("%s/api/v1/query?query=%s", url, encodedQuery)
	// Esegui la richiesta HTTP
	resp, err := http.Get(fullURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Controllo sullo stato della risposta HTTP
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Prometheus query failed: %s", resp.Status)
	}

	// Decodifica la risposta JSON
	var result struct {
		Data struct {
			Result []struct {
				Value []interface{} `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	// Estrai i valori dalla risposta JSON
	var values []float64
	for _, res := range result.Data.Result {
		if len(res.Value) > 1 {
			if v, err := strconv.ParseFloat(res.Value[1].(string), 64); err == nil {
				values = append(values, v)
			}
		}
	}

	return values, nil
}

func (r *ScalerReconciler) scalePod(ctx context.Context, pod *corev1.Pod, scale int32) error {
	logger := log.FromContext(ctx)
	logger.Info("Inizio il processo di scaling per il pod", "pod", pod.Name)
	image := pod.Spec.Containers[0].Image
	ownerName, ownerKind := getPodOwner(pod, logger)
	// Se il pod è gestito da un ReplicaSet, controlliamo se è a sua volta gestito da un Deployment
	if ownerKind == "ReplicaSet" {
		if deploymentName, found := getDeploymentFromReplicaSet(ctx, r, pod.Namespace, ownerName, logger); found {
			ownerName, ownerKind = deploymentName, "Deployment"
		}
	}
	// Se scale è negativo, non entrare nel caso default
	if scale < 0 {
		switch ownerKind {
		case "Deployment", "ReplicaSet", "StatefulSet":
			if err := scaleController(ctx, r, pod, ownerName, ownerKind, scale, logger); err != nil {
				return err
			}
			return nil

		default:
			logger.Info(fmt.Sprintf("%s non supporta la scalabilità, nessuna azione necessaria", ownerKind), "owner", ownerName)
			return nil
		}
	}
	// Caso per incrementare (scale > 0), quindi entra nel caso default
	switch ownerKind {
	case "Deployment", "ReplicaSet", "StatefulSet":
		if err := scaleController(ctx, r, pod, ownerName, ownerKind, scale, logger); err != nil {
			return err
		}
		return nil

	case "DaemonSet", "Job":
		logger.Info(fmt.Sprintf("%s non supporta la scalabilità, nessuna azione necessaria", ownerKind), "owner", ownerName)
		return nil

	default:
		return createPod(ctx, r, pod, image, logger)
	}
}

func scaleController(ctx context.Context, r *ScalerReconciler, pod *corev1.Pod, ownerName, ownerKind string, scale int32, logger logr.Logger) error {
	var replicas *int32

	lock, err := r.acquireLock(ctx, pod.Namespace, pod.Name)

	if err != nil {
		logger.Error(err, "Errore nell'acquisizione del lock per il pod", "pod", pod.Name)
		return err
	}
	if lock == nil {
		logger.Info("Lock non acquisito, saltando il pod", "pod", pod.Name)
		return nil
	}

	switch ownerKind {
	case "Deployment":
		var deployment appsv1.Deployment
		if err := r.Get(ctx, client.ObjectKey{Name: ownerName, Namespace: pod.Namespace}, &deployment); err != nil {
			return fmt.Errorf("errore nel recupero del Deployment %s: %w", ownerName, err)
		}
		replicas = deployment.Spec.Replicas
		annotateInitialReplicas(pod, *replicas, logger, r.Client, ctx)
		*replicas += scale
		if err := r.Update(ctx, &deployment); err != nil {
			return fmt.Errorf("errore nell'aggiornamento del Deployment %s: %w", ownerName, err)
		}
		// 🔹 Riavvia il Deployment dopo lo scaling
		if err := restartDeployment(ctx, r, ownerName, pod.Namespace, logger); err != nil {
			return fmt.Errorf("errore nel riavvio del Deployment %s: %w", ownerName, err)
		}
	case "ReplicaSet":
		var replicaSet appsv1.ReplicaSet
		if err := r.Get(ctx, client.ObjectKey{Name: ownerName, Namespace: pod.Namespace}, &replicaSet); err != nil {
			return fmt.Errorf("errore nel recupero del ReplicaSet %s: %w", ownerName, err)
		}
		replicas = replicaSet.Spec.Replicas
		annotateInitialReplicas(pod, *replicas, logger, r.Client, ctx)
		*replicas += scale
		if err := r.Update(ctx, &replicaSet); err != nil {
			return fmt.Errorf("errore nell'aggiornamento del ReplicaSet %s: %w", ownerName, err)
		}
		// 🔹 Riavvia il ReplicaSet dopo lo scaling
		if err := restartReplicaSet(ctx, r, ownerName, pod.Namespace, logger); err != nil {
			return fmt.Errorf("errore nel riavvio del ReplicaSet %s: %w", ownerName, err)
		}
	case "StatefulSet":
		var statefulSet appsv1.StatefulSet
		if err := r.Get(ctx, client.ObjectKey{Name: ownerName, Namespace: pod.Namespace}, &statefulSet); err != nil {
			return fmt.Errorf("errore nel recupero dello StatefulSet %s: %w", ownerName, err)
		}
		replicas = statefulSet.Spec.Replicas
		annotateInitialReplicas(pod, *replicas, logger, r.Client, ctx)
		*replicas += scale
		if err := r.Update(ctx, &statefulSet); err != nil {
			return fmt.Errorf("errore nell'aggiornamento dello StatefulSet %s: %w", ownerName, err)
		}
		// 🔹 Riavvia lo StatefulSet dopo lo scaling
		if err := restartStatefulSet(ctx, r, ownerName, pod.Namespace, logger); err != nil {
			return fmt.Errorf("errore nel riavvio dello StatefulSet %s: %w", ownerName, err)
		}
	}
	err = r.releaseLock(ctx, pod.Namespace, pod.Name)
	if err != nil {
		logger.Error(err, "Errore nel rilascio del lock per il pod", "pod", pod.Name)
	}
	if replicas != nil {
		logger.Info("Numero di repliche aggiornato", "ownerKind", ownerKind, "ownerName", ownerName, "newReplicas", *replicas)
	}

	return nil
}

func restartDeployment(ctx context.Context, r *ScalerReconciler, name, namespace string, logger logr.Logger) error {
	var deployment appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &deployment); err != nil {
		return fmt.Errorf("errore nel recupero del Deployment %s: %w", name, err)
	}

	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = make(map[string]string)
	}
	deployment.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)

	if err := r.Update(ctx, &deployment); err != nil {
		return fmt.Errorf("errore nell'aggiornamento del Deployment %s: %w", name, err)
	}

	logger.Info("Deployment riavviato", "name", name)
	return nil
}

func restartReplicaSet(ctx context.Context, r *ScalerReconciler, name, namespace string, logger logr.Logger) error {
	var replicaSet appsv1.ReplicaSet
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &replicaSet); err != nil {
		return fmt.Errorf("errore nel recupero del ReplicaSet %s: %w", name, err)
	}

	if replicaSet.Spec.Template.Annotations == nil {
		replicaSet.Spec.Template.Annotations = make(map[string]string)
	}
	replicaSet.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)

	if err := r.Update(ctx, &replicaSet); err != nil {
		return fmt.Errorf("errore nell'aggiornamento del ReplicaSet %s: %w", name, err)
	}

	logger.Info("ReplicaSet riavviato", "name", name)
	return nil
}

func restartStatefulSet(ctx context.Context, r *ScalerReconciler, name, namespace string, logger logr.Logger) error {
	var statefulSet appsv1.StatefulSet
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, &statefulSet); err != nil {
		return fmt.Errorf("errore nel recupero dello StatefulSet %s: %w", name, err)
	}

	if statefulSet.Spec.Template.Annotations == nil {
		statefulSet.Spec.Template.Annotations = make(map[string]string)
	}
	statefulSet.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)

	if err := r.Update(ctx, &statefulSet); err != nil {
		return fmt.Errorf("errore nell'aggiornamento dello StatefulSet %s: %w", name, err)
	}

	logger.Info("StatefulSet riavviato", "name", name)
	return nil
}

// Recupera il proprietario di un pod
func getPodOwner(pod *corev1.Pod, logger logr.Logger) (string, string) {
	if len(pod.OwnerReferences) > 0 {
		owner := pod.OwnerReferences[0]
		logger.Info("Trovato owner per il pod", "ownerName", owner.Name, "ownerKind", owner.Kind)
		return owner.Name, owner.Kind
	}
	logger.Info("Il pod non ha owner.")
	return "", ""
}

// Controlla se un ReplicaSet è gestito da un Deployment
func getDeploymentFromReplicaSet(ctx context.Context, r *ScalerReconciler, namespace, replicaSetName string, logger logr.Logger) (string, bool) {
	var replicaSet appsv1.ReplicaSet
	if err := r.Get(ctx, client.ObjectKey{Name: replicaSetName, Namespace: namespace}, &replicaSet); err != nil {
		logger.Error(err, "Errore nel recupero del ReplicaSet", "replicaSet", replicaSetName)
		return "", false
	}
	for _, owner := range replicaSet.OwnerReferences {
		if owner.Kind == "Deployment" {
			logger.Info("Il ReplicaSet è gestito da un Deployment", "replicaSet", replicaSetName, "deployment", owner.Name)
			return owner.Name, true
		}
	}
	return "", false
}

func annotateInitialReplicas(pod *corev1.Pod, initialReplicas int32, logger logr.Logger, r client.Client, ctx context.Context) {
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}

	if _, exists := pod.Annotations["scaler-operator/initial-replicas"]; !exists {
		pod.Annotations["scaler-operator/initial-replicas"] = fmt.Sprintf("%d", initialReplicas)
		logger.Info("Impostata annotazione initial-replicas", "pod", pod.Name, "replicas", initialReplicas)

		// Aggiorna il pod con la nuova annotazione
		if err := r.Update(ctx, pod); err != nil {
			logger.Error(err, "Errore nell'aggiornamento delle annotazioni del pod", "pod", pod.Name)
		}
	} else {
		logger.Info("L'annotazione initial-replicas esiste già, non viene sovrascritta", "pod", pod.Name)
	}
}

// Crea un nuovo pod
func createPod(ctx context.Context, r *ScalerReconciler, pod *corev1.Pod, image string, logger logr.Logger) error {
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: pod.Name + "-scaled-",
			Namespace:    pod.Namespace,
			Annotations: map[string]string{
				"scaler-operator/timestamp": time.Now().Format(time.RFC3339),
			},
			Labels: map[string]string{
				"scaler-operator/original-pod": pod.Name,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "scaler-container", Image: image},
			},
		},
	}

	if err := r.Create(ctx, newPod); err != nil {
		return fmt.Errorf("errore nella creazione del nuovo pod: %w", err)
	}

	return nil
}

// Helper per creare un puntatore a int32
func int32Ptr(i int32) *int32 { return &i }

func float64SliceToInterfaceSlice(input []float64) []interface{} {
	result := make([]interface{}, len(input))
	for i, v := range input {
		//		result[i] = v
		result[i] = float64(v)
	}
	return result
}

func queryPrometheusStatistics(url, namespace, podName, nodeName string) (float64, float64, error) {
	// Costruzione dinamica delle query Prometheus per ogni metrica
	cpuQueryAvg := fmt.Sprintf(`avg_over_time((container_cpu_usage_seconds_total{namespace="%s", pod=~"%s", kubernetes_io_hostname="%s"}[5m]))`, namespace, podName, nodeName)
	memQueryAvg := fmt.Sprintf(`avg_over_time(container_memory_usage_bytes{namespace="%s", pod=~"%s", kubernetes_io_hostname="%s"}[5m])`, namespace, podName, nodeName)
	// Eseguiamo le query Prometheus per ciascun valore
	cpuAvgData, err := executePrometheusQuery(url, cpuQueryAvg)
	if err != nil {
		return 0, 0, fmt.Errorf("errore nella query per la media CPU: %w", err)
	}

	avgCPU := calculateLastValue(float64SliceToInterfaceSlice(cpuAvgData)) 

	memAvgData, err := executePrometheusQuery(url, memQueryAvg)
	if err != nil {
		return 0, 0, fmt.Errorf("errore nella query per la media Memoria: %w", err)
	}

	avgMem := calculateLastValue(float64SliceToInterfaceSlice(memAvgData)) / (1024 * 1024)

	// Restituisce i valori aggregati
	return avgCPU, avgMem, nil
}

func calculateLastValue(data []interface{}) float64 {
	// Considera solo l'ultimo valore nella lista dei dati
	if len(data) > 0 {
		// Usa solo l'ultimo elemento dei dati
		switch v := data[len(data)-1].(type) {
		case map[string]interface{}:
			// Se è una mappa, estrai il valore
			value := v["value"].([]interface{})
			val, err := strconv.ParseFloat(value[1].(string), 64)
			if err == nil {
				return val
			}
		case float64:
			// Se è già un float64, restituisci direttamente
			return v
		}
	}
	return 0
}

func (r *ScalerReconciler) findSchedulableNodesForPod(
	ctx context.Context,
	pod corev1.Pod,
	log logr.Logger) ([]string, error) {

	// Recupera la lista dei nodi
	nodes, err := r.getNodes(ctx)
	if err != nil {
		log.Error(err, "Errore nel recupero dei nodi")
		return nil, err
	}

	var schedulableNodes []*corev1.Node

	// Itera attraverso tutti i nodi disponibili
	for _, node := range nodes {
		// Escludi i nodi Control Plane
		if _, isControlPlane := node.Labels["node-role.kubernetes.io/control-plane"]; isControlPlane {
			log.Info("Escludo nodo Control Plane", "node", node.Name)
			continue
		}

		nodeCPUSec, nodeMemBytes := r.getNodeResourceCapacity(&node)

		// Recupera il totale delle richieste già allocate nel nodo
		nodeCPUUsed, nodeMemUsed := r.getNodeResourceUsage(ctx, node.Name)

		// Risorse effettivamente disponibili
		nodeCPUUsedSec := float64(nodeCPUUsed) * 300 / 1000 // Conversione in secondi
		nodeCPUSecFree := nodeCPUSec - nodeCPUUsedSec       // CPU libera in secondi
		nodeMemBytesFree := nodeMemBytes - float64(nodeMemUsed)

		// Recupera le richieste di risorse del pod
		var podCPUMilli int64
		var podMemBytes int64

		for _, container := range pod.Spec.Containers {
			podCPUMilli += container.Resources.Requests.Cpu().MilliValue()
			podMemBytes += container.Resources.Requests.Memory().Value()
		}
		// Calcola la richiesta di CPU del pod in secondi (tenendo conto che 1 CPU = 1000 milliCPU)
		podCPUSec := float64(podCPUMilli) / 1000 // CPU richiesta in secondi

		// Verifica se il nodo ha abbastanza risorse libere
		if  nodeCPUSecFree >= (podCPUSec + (podCPUSec / 10)) && float64(nodeMemBytesFree) >= (float64(podMemBytes) + (float64(podMemBytes) / 10)) {
			log.Info("Nodo schedulabile", "node", node.Name)
			schedulableNodes = append(schedulableNodes, &node)
		} else {
			log.Info("Nodo non schedulabile", "node", node.Name)
		}
	}

	// Se nessun nodo è schedulabile, restituisci errore
	if len(schedulableNodes) == 0 {
		log.Info("Nessun nodo schedulabile trovato", "pod", pod.Name)
		return nil, fmt.Errorf("nessun nodo schedulabile trovato per il pod %s", pod.Name)
	}

	// Ritorna la lista dei nodi validi con nome e IP
	var result []string
	for _, node := range schedulableNodes {
		nodeName := node.Name
		var nodeIP string
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				nodeIP = addr.Address
				break
			}
		}
		result = append(result, fmt.Sprintf("Node Name: %s, Node IP: %s", nodeName, nodeIP))
	}

	return result, nil
}

// **Funzione per ottenere le richieste risorse su un nodo**
func (r *ScalerReconciler) getNodeResourceUsage(ctx context.Context, nodeName string) (int64, int64) {
	pods, err := r.getPodsForNode(ctx, nodeName)
	if err != nil {
		return 0, 0
	}

	var totalCPUMilli int64
	var totalMemBytes int64

	for _, pod := range pods {
		for _, container := range pod.Spec.Containers {
			totalCPUMilli += container.Resources.Requests.Cpu().MilliValue()
			totalMemBytes += container.Resources.Requests.Memory().Value()
		}
	}

	return totalCPUMilli, totalMemBytes
}

func (r *ScalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := mgr.GetFieldIndexer().IndexField(context.TODO(), &corev1.Pod{}, "spec.nodeName", func(rawObj client.Object) []string {
		pod := rawObj.(*corev1.Pod)
		if pod.Spec.NodeName == "" {
			return nil
		}
		return []string{pod.Spec.NodeName}
	})
	if err != nil {
		return err
	}

	// Setup MySQL configuration at the start
	if err := SetupMySQLConfig(); err != nil {
		return fmt.Errorf("failed to set up MySQL: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&scalerapi.Scaler{}).
		Complete(r)
}






