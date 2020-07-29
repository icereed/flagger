package router

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-cmp/cmp"
	"github.com/zalando/skipper/eskip"
	"github.com/zalando/skipper/predicates/traffic"
	"go.uber.org/zap"
	"k8s.io/api/networking/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"

	flaggerv1 "github.com/weaveworks/flagger/pkg/apis/flagger/v1beta1"
)

const skipperAnnotationPrefix = "zalando.org"
const (
	ingressRouteIDPrefix                = "kube"
	backendWeightsAnnotationKey         = "zalando.org/backend-weights"
	ratelimitAnnotationKey              = "zalando.org/ratelimit"
	skipperfilterAnnotationKey          = "zalando.org/skipper-filter"
	skipperpredicateAnnotationKey       = "zalando.org/skipper-predicate"
	skipperRoutesAnnotationKey          = "zalando.org/skipper-routes"
	skipperLoadBalancerAnnotationKey    = "zalando.org/skipper-loadbalancer"
	skipperBackendProtocolAnnotationKey = "zalando.org/skipper-backend-protocol"
	pathModeAnnotationKey               = "zalando.org/skipper-ingress-path-mode"
	ingressOriginName                   = "ingress"
	defaultCanaryCookieName             = "canary"
)

type SkipperRouter struct {
	kubeClient kubernetes.Interface
	logger     *zap.SugaredLogger
}

func (skp *SkipperRouter) Reconcile(canary *flaggerv1.Canary) error {
	//https://github.com/zalando/skipper/blob/dd70bd65e7f99cfb5dd6b6f71885d9fe3b2707f6/dataclients/kubernetes/ingress.go#L598
	var annotationFilter string
	eskip.ParseFilters(annotationFilter)
	if canary.Spec.IngressRef == nil || canary.Spec.IngressRef.Name == "" {
		return fmt.Errorf("ingress selector is empty")
	}

	apexName, _, _ := canary.GetServiceNames()
	canaryName := fmt.Sprintf("%s-canary", apexName)
	canaryIngressName := fmt.Sprintf("%s-canary", canary.Spec.IngressRef.Name)

	ingress, err := skp.kubeClient.NetworkingV1beta1().Ingresses(canary.Namespace).Get(context.TODO(), canary.Spec.IngressRef.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("ingress %s.%s get query error: %w", canary.Spec.IngressRef.Name, canary.Namespace, err)
	}

	iClone := ingress.DeepCopy()

	// change backend to <deployment-name>-canary
	backendExists := false
	for k, v := range iClone.Spec.Rules {
		for x, y := range v.HTTP.Paths {
			if y.Backend.ServiceName == apexName {
				iClone.Spec.Rules[k].HTTP.Paths[x].Backend.ServiceName = canaryName
				backendExists = true
			}
		}
	}

	if !backendExists {
		return fmt.Errorf("backend %s not found in ingress %s", apexName, canary.Spec.IngressRef.Name)
	}

	canaryIngress, err := skp.kubeClient.NetworkingV1beta1().Ingresses(canary.Namespace).Get(context.TODO(), canaryIngressName, metav1.GetOptions{})

	if errors.IsNotFound(err) {
		ing := &v1beta1.Ingress{
			ObjectMeta: metav1.ObjectMeta{
				Name:      canaryIngressName,
				Namespace: canary.Namespace,
				OwnerReferences: []metav1.OwnerReference{
					*metav1.NewControllerRef(canary, schema.GroupVersionKind{
						Group:   flaggerv1.SchemeGroupVersion.Group,
						Version: flaggerv1.SchemeGroupVersion.Version,
						Kind:    flaggerv1.CanaryKind,
					}),
				},
				Annotations: skp.makeAnnotations(iClone.Annotations),
				Labels:      iClone.Labels,
			},
			Spec: iClone.Spec,
		}

		_, err := skp.kubeClient.NetworkingV1beta1().Ingresses(canary.Namespace).Create(context.TODO(), ing, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("ingress %s.%s create error: %w", ing.Name, ing.Namespace, err)
		}

		skp.logger.With("canary", fmt.Sprintf("%s.%s", canary.Name, canary.Namespace)).
			Infof("Ingress %s.%s created", ing.GetName(), canary.Namespace)
		return nil
	} else if err != nil {
		return fmt.Errorf("ingress %s.%s query error: %w", canaryIngressName, canary.Namespace, err)
	}

	if diff := cmp.Diff(iClone.Spec, canaryIngress.Spec); diff != "" {
		iClone := canaryIngress.DeepCopy()
		iClone.Spec = iClone.Spec

		_, err := skp.kubeClient.NetworkingV1beta1().Ingresses(canary.Namespace).Update(context.TODO(), iClone, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("ingress %s.%s update error: %w", canaryIngressName, iClone.Namespace, err)
		}

		skp.logger.With("canary", fmt.Sprintf("%s.%s", canary.Name, canary.Namespace)).
			Infof("Ingress %s updated", canaryIngressName)
	}

	return nil
}

func (skp *SkipperRouter) GetRoutes(canary *flaggerv1.Canary) (primaryWeight, canaryWeight int, mirrored bool, err error) {
	canaryIngressName := fmt.Sprintf("%s-canary", canary.Spec.IngressRef.Name)
	canaryIngress, err := skp.kubeClient.NetworkingV1beta1().Ingresses(canary.Namespace).Get(context.TODO(), canaryIngressName, metav1.GetOptions{})
	if err != nil {
		err = fmt.Errorf("ingress %s.%s get query error: %w", canaryIngressName, canary.Namespace, err)
		return
	}

	annotation, ok := canaryIngress.Annotations[skipperpredicateAnnotationKey]
	if !ok || annotation == "" {
		err = fmt.Errorf("ingress %s.%s missing predicate annotation", canaryIngressName, canary.Namespace)
		return
	}
	predicates, err := eskip.ParsePredicates(annotation)
	if err != nil {
		err = fmt.Errorf("ingress %s.%s failed parsing '%s' as predicates: %w", canaryIngressName, canary.Namespace, annotation, err)
		return
	}
	found := false
	for _, p := range predicates {
		if p.Name == traffic.PredicateName {
			chance, ok := p.Args[0].(float64)
			if !ok || chance < 0.0 || chance > 1.0 {
				err = fmt.Errorf("ingress %s.%s failed to cast '%v'", canaryIngressName, canary.Namespace, p.Args[0])
				return
			}
			canaryWeight = int(chance * 100.0)
			found = true
		}
	}
	if !found {
		err = fmt.Errorf("ingress %s.%s predicate 'Traffic' missing on the canary ingress", canaryIngressName, canary.Namespace)
		return
	}

	primaryWeight = 100 - canaryWeight
	mirrored = false
	return
}

func (skp *SkipperRouter) SetRoutes(canary *flaggerv1.Canary, _ int, canaryWeight int, _ bool) (err error) {
	canaryIngressName := fmt.Sprintf("%s-canary", canary.Spec.IngressRef.Name)
	canaryIngress, err := skp.kubeClient.NetworkingV1beta1().Ingresses(canary.Namespace).Get(context.TODO(), canaryIngressName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("ingress %s.%s get query error: %w", canaryIngressName, canary.Namespace, err)
	}

	iClone := canaryIngress.DeepCopy()

	// TODO: A/B testing

	// Canary
	annotation, ok := canaryIngress.Annotations[skipperpredicateAnnotationKey]
	if !ok || annotation == "" {
		err = fmt.Errorf("ingress %s.%s missing predicate annotation", canaryIngressName, canary.Namespace)
		return
	}
	predicates, err := eskip.ParsePredicates(annotation)
	if err != nil {
		err = fmt.Errorf("ingress %s.%s failed parsing '%s' as predicates: %w", canaryIngressName, canary.Namespace, annotation, err)
		return
	}
	found := false
	for _, p := range predicates {
		if p.Name == traffic.PredicateName {
			found = true
			// adjust the chance definition 0.0 - 1.0
			p.Args[0] = float64(canaryWeight) / 100.0
		}
	}
	if !found {
		err = fmt.Errorf("ingress %s.%s predicate 'Traffic' missing on the canary ingress", canaryIngressName, canary.Namespace)
		return
	}

	// TODO: find a safer way to render predicate annotation
	predicateStr := strings.TrimSuffix((&eskip.Route{Predicates: predicates}).String(), ` -> ""`)

	iClone.Annotations[skipperpredicateAnnotationKey] = predicateStr

	_, err = skp.kubeClient.NetworkingV1beta1().Ingresses(canary.Namespace).Update(context.TODO(), iClone, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("ingress %s.%s update error %v", iClone.Name, iClone.Namespace, err)
	}

	return nil
}

func (skp *SkipperRouter) makeAnnotations(annotations map[string]string) map[string]string {
	res := make(map[string]string)
	for k, v := range annotations {
		if !strings.Contains(k, skp.annotationWithPrefix("canary")) &&
			!strings.Contains(k, "kubectl.kubernetes.io/last-applied-configuration") {
			res[k] = v
		}
	}

	res[skp.annotationWithPrefix("canary")] = "false"

	return res
}

func (skp *SkipperRouter) makeHeaderAnnotations(annotations map[string]string,
	header string, headerValue string, headerRegex string, cookie string) map[string]string {
	res := make(map[string]string)
	for k, v := range annotations {
		if !strings.Contains(v, skp.annotationWithPrefix("canary")) {
			res[k] = v
		}
	}

	res[skp.annotationWithPrefix("canary")] = "true"

	if cookie != "" {
		res[skp.annotationWithPrefix("canary-by-cookie")] = cookie
	}

	if header != "" {
		res[skp.annotationWithPrefix("canary-by-header")] = header
	}

	if headerValue != "" {
		res[skp.annotationWithPrefix("canary-by-header-value")] = headerValue
	}

	if headerRegex != "" {
		res[skp.annotationWithPrefix("canary-by-header-pattern")] = headerRegex
	}

	return res
}

func (skp *SkipperRouter) annotationWithPrefix(suffix string) string {
	return fmt.Sprintf("%v/%v", skipperAnnotationPrefix, suffix)
}

func (skp *SkipperRouter) Finalize(_ *flaggerv1.Canary) error {
	return nil
}
