/*
Copyright 2025.

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

package controller

import (
	"context"
	"fmt"
	"log"
	"reflect"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	autoscalev1 "cicd.operator/autoscale/api/v1"
)

// QueueItemWithDependency defines an item pushed into the queue
type QueueItemWithDependency struct {
	Id         string              `json:"id"`
	Dependency map[string][]string `json:"dependency"`
}

// RabbitMQConnection holds the connection and channel
type RabbitMQConnection struct {
	Conn    *amqp.Connection
	Channel *amqp.Channel
}

// RedisConnection holds the Redis client connection
type RedisConnection struct {
	Client  *redis.Client
	Context context.Context
}

// WorkloadAutoscaleReconciler reconciles a WorkloadAutoscale object
type WorkloadAutoscaleReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Clientset *kubernetes.Clientset
}

// +kubebuilder:rbac:groups=autoscale.cicd.operator,resources=workloadautoscales,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscale.cicd.operator,resources=workloadautoscales/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscale.cicd.operator,resources=workloadautoscales/finalizers,verbs=update
// +kubebuilder:rbac:groups=hpa.cicd.operator,resources=services;endpoints,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the WorkloadAutoscale object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *WorkloadAutoscaleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	// Fetch the autoscaler instance
	// The purpose is check if the Custom Resource for the Kind autoscaler
	// is applied on the cluster if not we return nil to stop the reconciliation
	autoscaler := &autoscalev1.WorkloadAutoscale{}
	if err := r.Get(ctx, req.NamespacedName, autoscaler); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Get RabbitMQ message count
	password, err := r.getSecretValue(autoscaler.Namespace, autoscaler.Spec.InputQueue.PasswordSecretRef)
	if err != nil {
		return r.handleError(ctx, autoscaler, "FailedToGetPassword", err)
	}

	messageCount, err := r.getQueueMessageCount(autoscaler, password)
	if err != nil {
		return r.handleError(ctx, autoscaler, "QueueCheckFailed", err)
	}

	// Update status with current queue size
	autoscaler.Status.QueueMessages = messageCount
	if err := r.Status().Update(ctx, autoscaler); err != nil {
		return ctrl.Result{}, err
	}

	return r.reconcileDeploymentScaling(ctx, autoscaler, messageCount, password)
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkloadAutoscaleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoscalev1.WorkloadAutoscale{}).
		Named("workloadautoscale").
		Complete(r)
}

func (r *WorkloadAutoscaleReconciler) reconcileDeploymentScaling(ctx context.Context, autoscaler *autoscalev1.WorkloadAutoscale, messageCount int32, password string) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1. Establish RabbitMQ connection
	rmqConn, err := r.connectRabbitMQ(autoscaler, password)
	if err != nil {
		return r.handleError(ctx, autoscaler, "RabbitMQConnectionFailed", err)
	}
	defer r.closeRabbitMQ(rmqConn)

	// 2. Process new messages polling from the queue
	for i := int32(0); i < messageCount; i++ {
		// Get message from queue
		msg, err := r.consumeMessage(rmqConn, autoscaler.Spec.InputQueue.QueueName)
		if err != nil {
			log.Error(err, "Failed to get message")
			continue
		}
		if msg == nil {
			break // No more messages
		}

		// Process one message
		if err := r.processSingleMessage(ctx, autoscaler, rmqConn.Channel, msg, i); err != nil {
			log.Error(err, "Failed to process message")
			continue
		}
	}

	return ctrl.Result{
		RequeueAfter: time.Duration(autoscaler.Spec.Scaling.PollingIntervalSeconds) * time.Second,
	}, nil
}

func (r *WorkloadAutoscaleReconciler) handleError(ctx context.Context, autoscaler *autoscalev1.WorkloadAutoscale, reason string, err error) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	autoscaler.Status.Phase = "Error"
	condition := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            err.Error(),
		LastTransitionTime: metav1.Now(),
	}
	autoscaler.Status.Conditions = append(autoscaler.Status.Conditions, condition)

	if updateErr := r.Status().Update(ctx, autoscaler); updateErr != nil {
		log.Error(updateErr, "Failed to update status")
		return ctrl.Result{}, updateErr
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, err
}

// Fetch svc Kubernetes endpoint of RabbitMQ
func (r *WorkloadAutoscaleReconciler) getServiceEndpoint(ctx context.Context, serviceHost string, instance *autoscalev1.WorkloadAutoscale) (string, error) {
	// log.Printf("getServiceEndpoint %s", serviceHost)
	// If host is already a full URL, use it directly
	if strings.Contains(serviceHost, ".") {
		return serviceHost, nil
	}

	// Get service details
	svc, err := r.Clientset.CoreV1().Services(instance.Namespace).Get(
		ctx,
		serviceHost, // "pipeline-queue" or "job-queue" or "minio"
		metav1.GetOptions{},
	)
	if err != nil {
		return "", fmt.Errorf("failed to get RabbitMQ service: %w", err)
	}

	// Handle different service types
	switch svc.Spec.Type {
	case corev1.ServiceTypeClusterIP:
		return fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, svc.Namespace), nil
	case corev1.ServiceTypeLoadBalancer:
		if len(svc.Status.LoadBalancer.Ingress) == 0 {
			return "", fmt.Errorf("LoadBalancer IP not yet allocated")
		}
		return svc.Status.LoadBalancer.Ingress[0].IP, nil
	case corev1.ServiceTypeNodePort:
		return fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, svc.Namespace), nil
	default:
		return "", fmt.Errorf("unsupported service type: %s", svc.Spec.Type)
	}
}

// Get secret values
func (r *WorkloadAutoscaleReconciler) getSecretValue(namespace string, secretRef autoscalev1.SecretReference) (string, error) {
	secret := &corev1.Secret{}
	err := r.Get(context.Background(),
		types.NamespacedName{
			Name:      secretRef.Name,
			Namespace: namespace,
		},
		secret,
	)
	if err != nil {
		return "", err
	}

	value, ok := secret.Data[secretRef.Key]
	if !ok {
		return "", fmt.Errorf("key %s not found in secret %s", secretRef.Key, secretRef.Name)
	}

	return string(value), nil
}

// Gets the current message count in the queue
func (r *WorkloadAutoscaleReconciler) getQueueMessageCount(autoscaler *autoscalev1.WorkloadAutoscale, password string) (int32, error) {
	// Establish connection
	rmq, err := r.connectRabbitMQ(autoscaler, password)
	if err != nil {
		return 0, err
	}
	defer r.closeRabbitMQ(rmq)

	// QueueDeclare declares a queue to hold messages and deliver to consumers.
	// Declaring creates a queue if it doesn't already exist,
	// or ensures that an existing queue matches the same parameters.
	queue, err := rmq.Channel.QueueDeclare(
		autoscaler.Spec.InputQueue.QueueName,
		true,  // durable
		false, // autoDelete
		false, // exclusive
		false, // noWait
		nil,   // arguments
	)
	if err != nil {
		return 0, fmt.Errorf("failed to declare queue: %w", err)
	}

	log.Printf("getQueueMessageCount %v", queue.Messages)
	return int32(queue.Messages), nil
}

// Establishes a new connection to RabbitMQ
func (r *WorkloadAutoscaleReconciler) connectRabbitMQ(autoscaler *autoscalev1.WorkloadAutoscale, password string) (*RabbitMQConnection, error) {
	host, err := r.getServiceEndpoint(context.Background(), autoscaler.Spec.InputQueue.Host, autoscaler)
	if err != nil {
		return nil, err
	}

	connString := fmt.Sprintf("amqp://%s:%s@%s:%d",
		autoscaler.Spec.InputQueue.Username,
		password,
		host,
		autoscaler.Spec.InputQueue.Port,
	)

	conn, err := amqp.Dial(connString)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to RabbitMQ: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close() // Close connection if channel creation fails
		return nil, fmt.Errorf("failed to open channel: %w", err)
	}

	return &RabbitMQConnection{
		Conn:    conn,
		Channel: ch,
	}, nil
}

// Safely closes the connection and channel
func (r *WorkloadAutoscaleReconciler) closeRabbitMQ(rmq *RabbitMQConnection) {
	if rmq == nil {
		return
	}

	if rmq.Channel != nil {
		rmq.Channel.Close()
	}
	if rmq.Conn != nil {
		rmq.Conn.Close()
	}
}

// Consumes a single message from the queue
func (r *WorkloadAutoscaleReconciler) consumeMessage(rmq *RabbitMQConnection, queueName string) (*amqp.Delivery, error) {
	msg, ok, err := rmq.Channel.Get(
		queueName,
		false, // auto-ack
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get message: %w", err)
	}
	if !ok {
		return nil, nil // No messages available
	}
	return &msg, nil
}

// Handles creation of pod for a single message
func (r *WorkloadAutoscaleReconciler) processSingleMessage(ctx context.Context, autoscaler *autoscalev1.WorkloadAutoscale, ch *amqp.Channel, msg *amqp.Delivery, index int32) error {
	log := logf.FromContext(ctx)

	// Create pod
	pod := r.createWorkerPod(autoscaler, index, msg.Body)
	if err := r.Create(ctx, pod); err != nil {
		// Requeue message on failure
		if nackErr := ch.Nack(msg.DeliveryTag, false, true); nackErr != nil {
			log.Error(nackErr, "Failed to requeue message after pod creation failure")
		}
		return fmt.Errorf("failed to create pod: %w", err)
	}

	// Acknowledge message
	if err := ch.Ack(msg.DeliveryTag, false); err != nil {
		return fmt.Errorf("failed to ack message: %w", err)
	}

	log.Info("Successfully processed message", "pod", pod.Name, "messageLength", len(msg.Body))
	return nil
}

func (r *WorkloadAutoscaleReconciler) createWorkerPod(autoscaler *autoscalev1.WorkloadAutoscale, index int32, messageBody []byte) *corev1.Pod {
	// Get service endpoints
	inputQueueHost, err := r.getServiceEndpoint(context.Background(), autoscaler.Spec.InputQueue.Host, autoscaler)
	if err != nil {
		logf.FromContext(context.Background()).Error(err, "Failed to get InputQueue Endpoint")
	}
	storageHost, err := r.getServiceEndpoint(context.Background(), autoscaler.Spec.Storage.Host, autoscaler)
	if err != nil {
		logf.FromContext(context.Background()).Error(err, "Failed to get MinIO Endpoint")
	}

	podName := fmt.Sprintf("%s-worker-%d-%d", autoscaler.Name, time.Now().Unix(), index)

	// Get secrets
	inputQueuePassword, _ := r.getSecretValue(autoscaler.Namespace, autoscaler.Spec.InputQueue.PasswordSecretRef)
	dbPassword, _ := r.getSecretValue(autoscaler.Namespace, autoscaler.Spec.Database.PasswordSecretRef)
	storageAccessKey, _ := r.getSecretValue(autoscaler.Namespace, autoscaler.Spec.Storage.AccessKeyRef)
	storageSecretKey, _ := r.getSecretValue(autoscaler.Namespace, autoscaler.Spec.Storage.SecretKeyRef)
	cachePassword, _ := r.getSecretValue(autoscaler.Namespace, autoscaler.Spec.Cache.PasswordSecretRef)

	// Worker environment variables
	envVars := []corev1.EnvVar{
		// InputQueue
		{
			Name: "RABBITMQ_URL",
			Value: fmt.Sprintf("amqp://%s:%s@%s:%d",
				autoscaler.Spec.InputQueue.Username,
				inputQueuePassword,
				inputQueueHost,
				autoscaler.Spec.InputQueue.Port,
			),
		},
		{Name: "TASK_QUEUE", Value: autoscaler.Spec.InputQueue.QueueName},
		// Database
		{Name: "DB_HOST", Value: autoscaler.Spec.Database.Host},
		{Name: "DB_PORT", Value: fmt.Sprintf("%d", autoscaler.Spec.Database.Port)},
		{Name: "DB_USER", Value: autoscaler.Spec.Database.Username},
		{Name: "DB_NAME", Value: autoscaler.Spec.Database.Name},
		{Name: "DB_SSL_MODE", Value: autoscaler.Spec.Database.SSLMode},
		{Name: "DB_PASSWORD", Value: dbPassword},
		{Name: "DB_SSL_CA", Value: "/etc/ssl/certs/ca.pem"},
		// Storage
		{Name: "MINIO_ENDPOINT", Value: storageHost},
		{Name: "MINIO_ACCESS_KEY", Value: storageAccessKey},
		{Name: "MINIO_SECRET_KEY", Value: storageSecretKey},
		{Name: "DEFAULT_BUCKET", Value: autoscaler.Spec.Storage.DefaultBucket},
		// Cache
		{Name: "REDIS_HOST", Value: autoscaler.Spec.Cache.Host},
		{Name: "REDIS_PORT", Value: fmt.Sprintf("%d", autoscaler.Spec.Cache.Port)},
		{Name: "REDIS_USERNAME", Value: autoscaler.Spec.Cache.Username},
		{Name: "REDIS_PASSWORD", Value: cachePassword},
	}

	// OutputQueue
	if !reflect.DeepEqual(autoscaler.Spec.OutputQueue, autoscalev1.RabbitMQConfig{}) {
		outputQueuePassword, _ := r.getSecretValue(autoscaler.Namespace, autoscaler.Spec.OutputQueue.PasswordSecretRef)

		envVars = append(envVars, corev1.EnvVar{
			Name: "JOB_QUEUE_URL",
			Value: fmt.Sprintf("amqp://%s:%s@%s:%d",
				autoscaler.Spec.OutputQueue.Username,
				outputQueuePassword,
				autoscaler.Spec.OutputQueue.Host,
				autoscaler.Spec.OutputQueue.Port,
			),
		})
		envVars = append(envVars, corev1.EnvVar{Name: "JOB_QUEUE_NAME", Value: autoscaler.Spec.OutputQueue.QueueName})
	}

	// Docker Volume
	var hostPathType corev1.HostPathType = corev1.HostPathSocket
	dockerVolume := corev1.Volume{
		Name: "docker-socket",
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: "/var/run/docker.sock", // Path on host to Docker socket
				Type: &hostPathType,
			},
		},
	}
	dockerVolumeMount := corev1.VolumeMount{
		Name:      "docker-socket",
		MountPath: "/var/run/docker.sock", // Mount Docker socket inside container
	}

	// SSL Volume
	sslVolume := corev1.Volume{
		Name: "ca-cert",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: autoscaler.Spec.Database.SSLCASecretRef.Name,
				Items: []corev1.KeyToPath{
					{
						Key:  autoscaler.Spec.Database.SSLCASecretRef.Key,
						Path: "ca.pem",
					},
				},
			},
		},
	}
	sslVolumeMount := corev1.VolumeMount{
		Name:      "ca-cert",
		MountPath: "/etc/ssl/certs/ca.pem",
		SubPath:   "ca.pem",
	}

	// Arguments
	args := []string{"--input", string(messageBody)}

	// Create pod with SSL and Docker volumes
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: autoscaler.Namespace,
			Labels: map[string]string{
				"app":                 "autoscaler-worker",
				"autoscaler-instance": autoscaler.Name,
				"pod-restart-policy":  "never", // never restart
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:         "worker",
					Image:        fmt.Sprintf("%s:%s", autoscaler.Spec.WorkerPool.WorkerImage, autoscaler.Spec.WorkerPool.WorkerImageTag),
					Args:         args,
					Env:          envVars,
					VolumeMounts: []corev1.VolumeMount{dockerVolumeMount, sslVolumeMount},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),  // 0.5 CPU
							corev1.ResourceMemory: resource.MustParse("256Mi"), // 256MB memory
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),  // 0.5 CPU
							corev1.ResourceMemory: resource.MustParse("512Mi"), // 256GB memory
						},
					},
				},
			},
			Volumes:                       []corev1.Volume{sslVolume, dockerVolume},
			TerminationGracePeriodSeconds: ptr.To(int64(30)),
		},
	}

	// Set owner reference
	if err := ctrl.SetControllerReference(autoscaler, pod, r.Scheme); err != nil {
		logf.FromContext(context.Background()).Error(err, "Failed to set owner reference")
	}

	return pod
}
