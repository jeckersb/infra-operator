/*
Copyright 2022.

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

package rabbitmq

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rabbitmqv1beta1 "github.com/openstack-k8s-operators/infra-operator/apis/rabbitmq/v1beta1"
	condition "github.com/openstack-k8s-operators/lib-common/modules/common/condition"
	helper "github.com/openstack-k8s-operators/lib-common/modules/common/helper"
	oko_secret "github.com/openstack-k8s-operators/lib-common/modules/common/secret"
	rabbitmqv1 "github.com/rabbitmq/cluster-operator/api/v1beta1"
	topology "github.com/rabbitmq/messaging-topology-operator/api/v1beta1"
	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// GetClient -
func (r *TransportURLReconciler) GetClient() client.Client {
	return r.Client
}

// GetKClient -
func (r *TransportURLReconciler) GetKClient() kubernetes.Interface {
	return r.Kclient
}

// GetLogger -
func (r *TransportURLReconciler) GetLogger() logr.Logger {
	return r.Log
}

// GetScheme -
func (r *TransportURLReconciler) GetScheme() *runtime.Scheme {
	return r.Scheme
}

// TransportURLReconciler reconciles a TransportURL object
type TransportURLReconciler struct {
	client.Client
	Kclient kubernetes.Interface
	Log     logr.Logger
	Scheme  *runtime.Scheme
}

//+kubebuilder:rbac:groups=rabbitmq.openstack.org,resources=transporturls,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rabbitmq.openstack.org,resources=transporturls/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=rabbitmq.openstack.org,resources=transporturls/finalizers,verbs=update
//+kubebuilder:rbac:groups=rabbitmq.com,resources=rabbitmqclusters,verbs=get;list;watch
//+kubebuilder:rbac:groups=rabbitmq.com,resources=vhosts,verbs=get;list;watch;create;update;patch;delete;
//+kubebuilder:rbac:groups=rabbitmq.com,resources=users,verbs=get;list;watch;create;update;patch;delete;
//+kubebuilder:rbac:groups=rabbitmq.com,resources=permissions,verbs=get;list;watch;create;update;patch;delete;
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete;

// Reconcile - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.12.2/pkg/reconcile
func (r *TransportURLReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, _err error) {
	_ = log.FromContext(ctx)

	// Fetch the TransportURL instance
	instance := &rabbitmqv1beta1.TransportURL{}
	err := r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		if k8s_errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	//
	// initialize status
	//
	if instance.Status.Conditions == nil {
		instance.Status.Conditions = condition.Conditions{}

		cl := condition.CreateList(condition.UnknownCondition(rabbitmqv1beta1.TransportURLReadyCondition, condition.InitReason, rabbitmqv1beta1.TransportURLReadyInitMessage))

		instance.Status.Conditions.Init(&cl)

		// Register overall status immediately to have an early feedback e.g. in the cli
		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	helper, err := helper.NewHelper(
		instance,
		r.Client,
		r.Kclient,
		r.Scheme,
		r.Log,
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Always patch the instance status when exiting this function so we can persist any changes.
	defer func() {
		// update the Ready condition based on the sub conditions
		if instance.Status.Conditions.AllSubConditionIsTrue() {
			instance.Status.Conditions.MarkTrue(
				condition.ReadyCondition, condition.ReadyMessage)
		} else {
			// something is not ready so reset the Ready condition
			instance.Status.Conditions.MarkUnknown(
				condition.ReadyCondition, condition.InitReason, condition.ReadyInitMessage)
			// and recalculate it based on the state of the rest of the conditions
			instance.Status.Conditions.Set(
				instance.Status.Conditions.Mirror(condition.ReadyCondition))
		}
		err := helper.PatchInstance(ctx, instance)
		if err != nil {
			_err = err
			return
		}
	}()

	return r.reconcileNormal(ctx, instance, helper)

}

func (r *TransportURLReconciler) reconcileNormal(ctx context.Context, instance *rabbitmqv1beta1.TransportURL, helper *helper.Helper) (ctrl.Result, error) {

	//TODO (implement a watch on the rabbitmq cluster resources to update things if there are changes)
	rabbit, err := getRabbitmqCluster(ctx, helper, instance)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Wait on RabbitmqCluster to be ready
	rabbitReady := false
	for _, condition := range rabbit.Status.Conditions {
		if condition.Reason == "AllPodsAreReady" && condition.Status == "True" {
			rabbitReady = true
			break
		}
	}
	if !rabbitReady {
		setErrorCondition(instance, nil)
		return ctrl.Result{RequeueAfter: time.Duration(10) * time.Second}, nil
	}

	vhostName := "/"

	if instance.Spec.Vhost != "" && instance.Spec.Vhost != "/" {
		vhostName = vhostName + instance.Spec.Vhost

		vhost := &topology.Vhost{
			ObjectMeta: metav1.ObjectMeta{
				Name:      instance.Spec.Vhost,
				Namespace: instance.Namespace,
			},
		}

		op, err := controllerutil.CreateOrUpdate(ctx, helper.GetClient(), vhost, func() error {
			vhost.Spec.Name = instance.Spec.Vhost
			vhost.Spec.RabbitmqClusterReference.Name = instance.Spec.RabbitmqClusterName

			if instance.Spec.QuorumQueues {
				vhost.Spec.DefaultQueueType = "quorum"
			}

			err := controllerutil.SetControllerReference(instance, vhost, r.Scheme)
			if err != nil {
				return err
			}

			return nil
		})

		if err != nil {
			return ctrl.Result{}, err
		}

		if op != controllerutil.OperationResultNone {
			helper.GetLogger().Info(fmt.Sprintf("Vhost %s - %s", vhost.Name, op))
		}
	}

	var username, password string

	if instance.Spec.User != "" {
		username = instance.Spec.User

		// the only way to provide a desired username instead
		// of getting a randomly-generated username is to
		// stick it in a secret and then pass that secret via
		// importCredentialsSecret.  By omitting the password,
		// the user will be given a randomly-generated
		// password
		usernameSecret := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "transporturl-" + instance.ObjectMeta.Name + "-username",
				Namespace: instance.ObjectMeta.Namespace,
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"username": []byte(username),
			},
		}

		op, err := controllerutil.CreateOrUpdate(ctx, helper.GetClient(), &usernameSecret, func() error {
			err := controllerutil.SetControllerReference(instance, &usernameSecret, r.Scheme)
			if err != nil {
				return err
			}

			return nil
		})

		if err != nil {
			return ctrl.Result{}, err
		}

		if op != controllerutil.OperationResultNone {
			helper.GetLogger().Info(fmt.Sprintf("Secret %s - %s", usernameSecret.ObjectMeta.Name, op))
		}

		user := topology.User{
			ObjectMeta: metav1.ObjectMeta{
				Name:      instance.Spec.User,
				Namespace: instance.ObjectMeta.Namespace,
			},
		}

		op, err = controllerutil.CreateOrUpdate(ctx, helper.GetClient(), &user, func() error {
			user.Spec.RabbitmqClusterReference.Name = instance.Spec.RabbitmqClusterName
			user.Spec.ImportCredentialsSecret = &corev1.LocalObjectReference{Name: usernameSecret.ObjectMeta.Name}
			err := controllerutil.SetControllerReference(instance, &user, r.Scheme)
			if err != nil {
				return err
			}

			return nil
		})

		if err != nil {
			return ctrl.Result{}, err
		}

		if op != controllerutil.OperationResultNone {
			helper.GetLogger().Info(fmt.Sprintf("User %s - %s", user.ObjectMeta.Name, op))
		}

		// get credential name out of user object so we can read the password from there
		updatedUser := &topology.User{}
		err = r.GetClient().Get(ctx, types.NamespacedName{Name: user.Name, Namespace: user.Namespace}, updatedUser)
		if err != nil || updatedUser.Status.Credentials == nil {
			return ctrl.Result{RequeueAfter: time.Duration(10) * time.Second}, nil
		}

		passwd, ctrlResult, err := oko_secret.GetDataFromSecret(ctx, helper, user.Status.Credentials.Name, time.Duration(10)*time.Second, "password")
		if err != nil {
			setErrorCondition(instance, err)
			return ctrl.Result{}, err
		} else if (ctrlResult != ctrl.Result{}) {
			return ctrlResult, nil
		}

		password = passwd

		permission := topology.Permission{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "transporturl-" + instance.ObjectMeta.Name,
				Namespace: instance.ObjectMeta.Namespace,
			},
		}

		op, err = controllerutil.CreateOrUpdate(ctx, helper.GetClient(), &permission, func() error {
			permission.Spec.RabbitmqClusterReference.Name = instance.Spec.RabbitmqClusterName
			permission.Spec.User = instance.Spec.User

			if instance.Spec.Vhost == "" {
				permission.Spec.Vhost = "/"
			} else {
				permission.Spec.Vhost = instance.Spec.Vhost
			}

			var configure, read, write string

			if instance.Spec.ConfigurePermissions == "" {
				configure = ".*"
			} else {
				configure = instance.Spec.ConfigurePermissions
			}

			if instance.Spec.ReadPermissions == "" {
				read = ".*"
			} else {
				read = instance.Spec.ReadPermissions
			}

			if instance.Spec.WritePermissions == "" {
				write = ".*"
			} else {
				write = instance.Spec.WritePermissions
			}

			permission.Spec.Permissions = topology.VhostPermissions{
				Configure: configure,
				Read:      read,
				Write:     write,
			}

			err := controllerutil.SetControllerReference(instance, &permission, r.Scheme)
			if err != nil {
				return err
			}

			return nil
		})

		if err != nil {
			return ctrl.Result{}, err
		}

		if op != controllerutil.OperationResultNone {
			helper.GetLogger().Info(fmt.Sprintf("Permission %s - %s", permission.ObjectMeta.Name, op))
		}

	} else {
		var (
			ctrlResult reconcile.Result
			err        error
		)

		username, ctrlResult, err = oko_secret.GetDataFromSecret(ctx, helper, rabbit.Status.DefaultUser.SecretReference.Name, time.Duration(10)*time.Second, "username")
		if err != nil {
			setErrorCondition(instance, err)
			return ctrl.Result{}, err
		} else if (ctrlResult != ctrl.Result{}) {
			return ctrlResult, nil
		}

		password, ctrlResult, err = oko_secret.GetDataFromSecret(ctx, helper, rabbit.Status.DefaultUser.SecretReference.Name, time.Duration(10)*time.Second, "password")
		if err != nil {
			setErrorCondition(instance, err)
			return ctrl.Result{}, err
		} else if (ctrlResult != ctrl.Result{}) {
			return ctrlResult, nil
		}

	}

	host, ctrlResult, err := oko_secret.GetDataFromSecret(ctx, helper, rabbit.Status.DefaultUser.SecretReference.Name, time.Duration(10)*time.Second, "host")
	if err != nil {
		setErrorCondition(instance, err)
		return ctrl.Result{}, err
	} else if (ctrlResult != ctrl.Result{}) {
		return ctrlResult, nil
	}

	// Create a new secret with the transport URL for this CR
	secret := r.createTransportURLSecret(instance, string(username), string(password), string(host), vhostName)
	_, op, err := oko_secret.CreateOrPatchSecret(ctx, helper, instance, secret)
	if err != nil {
		setErrorCondition(instance, err)
		return ctrl.Result{}, err
	}
	if op != controllerutil.OperationResultNone {
		setErrorCondition(instance, err)
		return ctrl.Result{RequeueAfter: time.Second * 5}, nil
	}

	// Update the CR and return
	instance.Status.SecretName = secret.Name

	instance.Status.Conditions.MarkTrue(rabbitmqv1beta1.TransportURLReadyCondition, rabbitmqv1beta1.TransportURLReadyMessage)

	return ctrl.Result{}, nil

}

// Create k8s secret with transport URL
func (r *TransportURLReconciler) createTransportURLSecret(instance *rabbitmqv1beta1.TransportURL, username string, password string, host string, vhost string) *corev1.Secret {
	// Create a new secret with the transport URL for this CR
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rabbitmq-transport-url-" + instance.Name,
			Namespace: instance.Namespace,
		},
		Data: map[string][]byte{
			"transport_url": []byte(fmt.Sprintf("rabbit://%s:%s@%s:5672%s", username, password, host, vhost)),
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *TransportURLReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rabbitmqv1beta1.TransportURL{}).
		Owns(&corev1.Secret{}).
		Owns(&topology.Vhost{}).
		Owns(&topology.User{}).
		Owns(&topology.Permission{}).
		Complete(r)
}

// GetRabbitmqCluster - get RabbitmqCluster object in namespace
func getRabbitmqCluster(
	ctx context.Context,
	h *helper.Helper,
	instance *rabbitmqv1beta1.TransportURL,
) (*rabbitmqv1.RabbitmqCluster, error) {
	rabbitMqCluster := &rabbitmqv1.RabbitmqCluster{}

	err := h.GetClient().Get(ctx, types.NamespacedName{Name: instance.Spec.RabbitmqClusterName, Namespace: instance.Namespace}, rabbitMqCluster)

	return rabbitMqCluster, err
}

func getRabbitmqVhostString(instance *rabbitmqv1beta1.TransportURL) string {
	if instance.Spec.Vhost == "" {
		return "/"
	}

	return instance.Spec.Vhost
}

// GetRabbitmqVhost - get Rabbitmq Vhost object in namespace
func getRabbitmqVhost(
	ctx context.Context,
	h *helper.Helper,
	instance *rabbitmqv1beta1.TransportURL,
) (*topology.Vhost, error) {
	vhost := &topology.Vhost{}

	err := h.GetClient().Get(ctx, types.NamespacedName{Name: getRabbitmqVhostString(instance), Namespace: instance.Namespace}, vhost)

	return vhost, err
}

// GetRabbitmqUser - get Rabbitmq User object in namespace
func getRabbitmqUser(
	ctx context.Context,
	h *helper.Helper,
	instance *rabbitmqv1beta1.TransportURL,
) (*topology.User, error) {
	user := &topology.User{}

	err := h.GetClient().Get(ctx, types.NamespacedName{Name: instance.Spec.User, Namespace: instance.Namespace}, user)

	return user, err
}

// GetRabbitmqPermission - get Rabbitmq Permission object in namespace
func getRabbitmqPermission(
	ctx context.Context,
	h *helper.Helper,
	instance *rabbitmqv1beta1.TransportURL,
) (*topology.Permission, error) {
	permission := &topology.Permission{}

	err := h.GetClient().Get(ctx, types.NamespacedName{Name: "placeholder", Namespace: instance.Namespace}, permission)

	return permission, err
}

// SetErrorCondition - Update instance Conditions to reflect error state
func setErrorCondition(
	instance *rabbitmqv1beta1.TransportURL,
	err error,
) {
	instance.Status.Conditions.Set(condition.FalseCondition(
		rabbitmqv1beta1.TransportURLReadyCondition,
		condition.ErrorReason,
		condition.SeverityWarning,
		rabbitmqv1beta1.TransportURLReadyErrorMessage,
		err.Error()))
}
