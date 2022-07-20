package provision

import (
	ispnv1 "github.com/infinispan/infinispan-operator/api/v1"
	"github.com/infinispan/infinispan-operator/api/v2alpha1"
	"github.com/infinispan/infinispan-operator/controllers/constants"
	kube "github.com/infinispan/infinispan-operator/pkg/kubernetes"
	pipeline "github.com/infinispan/infinispan-operator/pkg/reconcile/pipeline/infinispan"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const InfinispanListenerContainer = "infinispan-listener"

func ConfigListener(i *ispnv1.Infinispan, ctx pipeline.Context) {
	if !i.IsConfigListenerEnabled() {
		RemoveConfigListener(i, ctx)
		return
	}

	var configListenerImage string
	if constants.ConfigListenerImageName != "" {
		configListenerImage = constants.ConfigListenerImageName
	} else {
		// If env not explicitly set, use the Operator image
		if image, err := kube.GetOperatorImage(ctx.Ctx(), ctx.Kubernetes().Client); err != nil {
			ctx.Log().Error(err, "unable to create ConfigListener deployment")
			ctx.Requeue(err)
			return
		} else {
			configListenerImage = image
		}
	}

	r := ctx.Resources()
	name := i.GetConfigListenerName()
	namespace := i.Namespace

	objectMeta := metav1.ObjectMeta{
		Name:      name,
		Namespace: namespace,
	}

	deployment := &appsv1.Deployment{}
	listenerExists := r.Load(name, deployment) == nil
	if listenerExists {
		container := kube.GetContainer(InfinispanListenerContainer, &deployment.Spec.Template.Spec)
		if container != nil && container.Image == configListenerImage {
			if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0 {
				if err := ScaleConfigListener(1, i, ctx); err != nil {
					ctx.Requeue(err)
					return
				}
			}
			// The Deployment already exists with the expected image and number of replicas, do nothing
			return
		}
	}

	createOrUpdate := func(obj client.Object) error {
		if listenerExists {
			return r.Update(obj, pipeline.RetryOnErr)
		} else {
			return r.Create(obj, true, pipeline.RetryOnErr)
		}
	}
	// Create a ServiceAccount in the cluster namespace so that the ConfigListener has the required API permissions
	sa := &corev1.ServiceAccount{
		ObjectMeta: objectMeta,
	}
	if err := createOrUpdate(sa); err != nil {
		return
	}

	role := &rbacv1.Role{
		ObjectMeta: objectMeta,
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{v2alpha1.GroupVersion.Group},
				Resources: []string{"caches"},
				Verbs: []string{
					"create",
					"delete",
					"get",
					"list",
					"patch",
					"update",
					"watch",
				},
			},
			{
				APIGroups: []string{ispnv1.GroupVersion.Group},
				Resources: []string{"infinispans"},
				Verbs:     []string{"get"},
			}, {
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"list"},
			}, {
				APIGroups: []string{""},
				Resources: []string{"pods/exec"},
				Verbs:     []string{"create"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"secrets"},
				Verbs:     []string{"get"},
			},
		},
	}
	if err := createOrUpdate(role); err != nil {
		return
	}

	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: objectMeta,
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     name,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      name,
			Namespace: namespace,
		}},
	}
	if err := createOrUpdate(roleBinding); err != nil {
		return
	}

	// The deployment doesn't exist, create it
	labels := i.PodLabels()
	labels["app"] = "infinispan-config-listener-pod"
	deployment = &appsv1.Deployment{
		ObjectMeta: objectMeta,
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  InfinispanListenerContainer,
							Image: configListenerImage,
							Args: []string{
								"listener",
								"-namespace",
								namespace,
								"-cluster",
								i.Name,
							},
						},
					},
					ServiceAccountName: name,
				},
			},
		},
	}
	if err := createOrUpdate(deployment); err != nil {
		return
	}
}

func RemoveConfigListener(i *ispnv1.Infinispan, ctx pipeline.Context) {
	resources := []client.Object{
		&appsv1.Deployment{},
		&rbacv1.Role{},
		&rbacv1.RoleBinding{},
		&corev1.ServiceAccount{},
	}

	name := i.GetConfigListenerName()
	for _, obj := range resources {
		if err := ctx.Resources().Delete(name, obj, pipeline.RetryOnErr, pipeline.IgnoreNotFound); err != nil {
			return
		}
	}
}

func ScaleConfigListener(replicas int32, i *ispnv1.Infinispan, ctx pipeline.Context) error {
	if !i.IsConfigListenerEnabled() {
		return nil
	}
	// Remove the ConfigListener deployment as no Infinispan Pods exist
	ctx.Log().Info("Scaling ConfigListener deployment", "replicas", replicas)

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      i.GetConfigListenerName(),
			Namespace: i.Namespace,
		},
	}

	_, err := ctx.Resources().CreateOrPatch(deployment, false, func() error {
		if deployment.CreationTimestamp.IsZero() {
			return errors.NewNotFound(appsv1.Resource("deployment"), deployment.Name)
		}
		deployment.Spec.Replicas = pointer.Int32Ptr(replicas)
		return nil
	})

	if err != nil {
		ctx.Log().Error(err, "unable to scale ConfigListener Deployment")
		return err
	}
	return nil
}