/*
Copyright 2022 The Knative Authors

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

package common

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
	"knative.dev/serving/pkg/apis/autoscaling"
	autoscalingv1alpha1 "knative.dev/serving/pkg/apis/autoscaling/v1alpha1"
	"knative.dev/serving/pkg/apis/serving"
	v1 "knative.dev/serving/pkg/apis/serving/v1"
	clientset "knative.dev/serving/pkg/client/clientset/versioned"
	servingclient "knative.dev/serving/pkg/client/injection/client"
	painformer "knative.dev/serving/pkg/client/injection/informers/autoscaling/v1alpha1/podautoscaler"
	revisioninformer "knative.dev/serving/pkg/client/injection/informers/serving/v1/revision"
	routeinformer "knative.dev/serving/pkg/client/injection/informers/serving/v1/route"
	serviceorchestratorinformer "knative.dev/serving/pkg/client/injection/informers/serving/v1/serviceorchestrator"
	spainformer "knative.dev/serving/pkg/client/injection/informers/serving/v1/stagepodautoscaler"
	palisters "knative.dev/serving/pkg/client/listers/autoscaling/v1alpha1"
	listers "knative.dev/serving/pkg/client/listers/serving/v1"
	spalisters "knative.dev/serving/pkg/client/listers/serving/v1"
	revisionResources "knative.dev/serving/pkg/reconciler/revision/resources"
	"knative.dev/serving/pkg/reconciler/revision/resources/names"
	"knative.dev/serving/pkg/reconciler/service/resources"
	resourcenames "knative.dev/serving/pkg/reconciler/service/resources/names"
	"knative.dev/serving/pkg/reconciler/serviceorchestrator"
)

// Extension enables pluggable extensive features added into the controller
type Extension interface {
	TransformRevision(context.Context, *v1.Revision) (*v1.Revision, error)
	PostRevisionReconcile(context.Context, *v1.Revision, *autoscalingv1alpha1.PodAutoscaler) error
	PostConfigurationReconcile(context.Context, *v1.Service) error
	TransformService(*v1.Service) *v1.Service
	UpdateExtensionStatus(context.Context, *v1.Service) (bool, error)
}

func NoExtension() Extension {
	return &nilExtension{}
}

type nilExtension struct {
}

func (nilExtension) TransformRevision(ctx context.Context, revision *v1.Revision) (*v1.Revision, error) {
	return revision, nil
}

func (nilExtension) PostRevisionReconcile(context.Context, *v1.Revision, *autoscalingv1alpha1.PodAutoscaler) error {
	return nil
}

func (nilExtension) PostConfigurationReconcile(context.Context, *v1.Service) error {
	return nil
}

func (nilExtension) TransformService(service *v1.Service) *v1.Service {
	return service
}

func (nilExtension) UpdateExtensionStatus(context.Context, *v1.Service) (bool, error) {
	return true, nil
}

func ProgressiveRolloutExtension(ctx context.Context) Extension {
	return &progressiveRolloutExtension{
		client:                    servingclient.Get(ctx),
		stagepodAutoscalerLister:  spainformer.Get(ctx).Lister(),
		serviceOrchestratorLister: serviceorchestratorinformer.Get(ctx).Lister(),
		revisionLister:            revisioninformer.Get(ctx).Lister(),
		routeLister:               routeinformer.Get(ctx).Lister(),
		podAutoscalerLister:       painformer.Get(ctx).Lister(),
	}
}

type progressiveRolloutExtension struct {
	client                    clientset.Interface
	stagepodAutoscalerLister  spalisters.StagePodAutoscalerLister
	serviceOrchestratorLister listers.ServiceOrchestratorLister
	revisionLister            listers.RevisionLister
	routeLister               listers.RouteLister
	podAutoscalerLister       palisters.PodAutoscalerLister
}

func (e *progressiveRolloutExtension) TransformRevision(ctx context.Context, revision *v1.Revision) (*v1.Revision, error) {
	ns := revision.Namespace
	paName := names.PA(revision)
	spa, err := e.stagepodAutoscalerLister.StagePodAutoscalers(ns).Get(paName)
	if err != nil {
		return revision, err
	}
	return makeRevC(revision, spa), nil
}

func (e *progressiveRolloutExtension) PostRevisionReconcile(ctx context.Context, revC *v1.Revision, pa *autoscalingv1alpha1.PodAutoscaler) error {
	tmpl := revisionResources.MakePA(revC)
	if !equality.Semantic.DeepEqual(tmpl.Annotations, pa.Annotations) {
		want := pa.DeepCopy()
		want.Annotations = tmpl.Annotations
		if _, err := e.client.AutoscalingV1alpha1().PodAutoscalers(pa.Namespace).Update(ctx, want, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("failed to update PA %q: %w", pa.Name, err)
		}
	}
	return nil
}

func (e *progressiveRolloutExtension) PostConfigurationReconcile(ctx context.Context, service *v1.Service) error {
	_, err := e.serviceOrchestrator(ctx, service)
	if err != nil {
		return err
	}
	_, err = e.serviceOrchestratorLister.ServiceOrchestrators(service.Namespace).Get(service.Name)
	if err != nil {
		return err
	}

	return nil
}

func (e *progressiveRolloutExtension) TransformService(service *v1.Service) *v1.Service {
	so, err := e.serviceOrchestratorLister.ServiceOrchestrators(service.Namespace).Get(service.Name)
	if err != nil {
		return service
	}
	return MakeRouteFromSo(service, so)
}

func (e *progressiveRolloutExtension) UpdateExtensionStatus(ctx context.Context, service *v1.Service) (bool, error) {
	so, err := e.serviceOrchestratorLister.ServiceOrchestrators(service.Namespace).Get(service.Name)
	if err != nil {
		return false, err
	}
	if so.IsReady() {
		service.Status.MarkServiceOrchestratorReady()
		return true, nil
	} else {
		service.Status.MarkServiceOrchestratorInProgress()
		if equality.Semantic.DeepEqual(so.Spec.RevisionTarget, so.Spec.StageRevisionTarget) || serviceorchestrator.LatestEqual(so.Spec.StageRevisionTarget, so.Spec.RevisionTarget) {
			// We reach the last stage, there is no need to schedule a requeue in 2 mins.
			return true, nil
		}

		targetTime := so.Spec.TargetFinishTime

		now := metav1.NewTime(time.Now())
		if targetTime.Inner.Before(&now) {
			// Check if the stage target time has expired. If so, change the traffic split to the next stage.
			stageRevisionTarget := shiftTrafficNextStage(so.Spec.StageRevisionTarget)
			so.Spec.StageRevisionTarget = stageRevisionTarget
			t := time.Now()
			so.Spec.StageTarget.TargetFinishTime.Inner = metav1.NewTime(t.Add(time.Minute * 2))
			e.client.ServingV1().ServiceOrchestrators(service.Namespace).Update(ctx, so, metav1.UpdateOptions{})
		}

		return false, nil
	}
}

func shiftTrafficNextStage(revisionTarget []v1.RevisionTarget) []v1.RevisionTarget {
	stageTrafficDeltaInt := int64(resources.OverSubRatio)
	for i, _ := range revisionTarget {
		if revisionTarget[i].Direction == "up" || revisionTarget[i].Direction == "" {
			if *revisionTarget[i].Percent+stageTrafficDeltaInt >= 100 {
				revisionTarget[i].Percent = ptr.Int64(100)
			} else {
				revisionTarget[i].Percent = ptr.Int64(*revisionTarget[i].Percent + stageTrafficDeltaInt)
			}
		} else if revisionTarget[i].Direction == "down" {
			if *revisionTarget[i].Percent-stageTrafficDeltaInt <= 0 {
				revisionTarget[i].Percent = ptr.Int64(0)
			} else {
				revisionTarget[i].Percent = ptr.Int64(*revisionTarget[i].Percent - stageTrafficDeltaInt)
			}
		}
	}

	return revisionTarget
}

func convertIntoTrafficTarget(service *v1.Service, revisionTarget []v1.RevisionTarget) []v1.TrafficTarget {
	trafficTarget := make([]v1.TrafficTarget, len(revisionTarget), len(revisionTarget))
	for i, revision := range revisionTarget {
		target := v1.TrafficTarget{}
		target.LatestRevision = revision.LatestRevision
		target.Percent = revision.Percent
		if *revision.LatestRevision {
			target.ConfigurationName = service.Name
		} else {
			target.RevisionName = revision.RevisionName
		}
		trafficTarget[i] = target
	}
	return trafficTarget
}

func MakeRouteFromSo(service *v1.Service, so *v1.ServiceOrchestrator) *v1.Service {
	trafficTarget := convertIntoTrafficTarget(service, so.Spec.StageRevisionTarget)
	service.Spec.RouteSpec = v1.RouteSpec{
		Traffic: trafficTarget,
	}
	// Fill in any missing ConfigurationName fields when translating
	// from Service to Route.
	for idx := range service.Spec.Traffic {
		if service.Spec.Traffic[idx].RevisionName == "" {
			service.Spec.Traffic[idx].ConfigurationName = resourcenames.Configuration(service)
		}
	}

	return service
}

func (e *progressiveRolloutExtension) serviceOrchestrator(ctx context.Context, service *v1.Service) (*v1.ServiceOrchestrator, error) {

	recorder := controller.GetEventRecorder(ctx)

	routeName := resourcenames.Route(service)
	route, err := e.routeLister.Routes(service.Namespace).Get(routeName)
	if apierrs.IsNotFound(err) {
		route = nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to get the route: %w", err)
	}

	soName := resourcenames.ServiceOrchestrator(service)
	so, err := e.serviceOrchestratorLister.ServiceOrchestrators(service.Namespace).Get(soName)
	if apierrs.IsNotFound(err) {
		so, err = e.createUpdateServiceOrchestrator(ctx, service, route, nil, true)
		if err != nil {
			recorder.Eventf(service, corev1.EventTypeWarning, "CreationFailed", "Failed to create ServiceOrchestrator %q: %v", soName, err)
			return nil, fmt.Errorf("failed to create ServiceOrchestrator: %w", err)
		}
		recorder.Eventf(service, corev1.EventTypeNormal, "Created", "Created ServiceOrchestrator %q", soName)
	} else if err != nil {
		return nil, fmt.Errorf("failed to get Configuration: %w", err)
	} else if !metav1.IsControlledBy(so, service) {
		// Surface an error in the service's status,and return an error.
		service.Status.MarkServiceOrchestratorNotOwned(soName)
		return nil, fmt.Errorf("service: %q does not own ServiceOrchestrator: %q", service.Name, soName)
	} else if so, err = e.reconcileServiceOrchestrator(ctx, service, route, so); err != nil {
		return nil, fmt.Errorf("failed to reconcile Configuration: %w", err)
	}

	return nil, nil
}

func (e *progressiveRolloutExtension) createUpdateServiceOrchestrator(ctx context.Context, service *v1.Service,
	route *v1.Route, so *v1.ServiceOrchestrator, create bool) (*v1.ServiceOrchestrator, error) {
	// To create the service, orchestrator, we need to make sure we have stageTraffic and Traffic in the spec, and
	// stageReady, and Ready in the status.
	records := map[string]resources.RevisionRecord{}

	lister := e.revisionLister.Revisions(service.Namespace)
	list, err := lister.List(labels.SelectorFromSet(labels.Set{
		serving.ConfigurationLabelKey: service.Name,
	}))

	logger := logging.FromContext(ctx)

	if err == nil && len(list) > 0 {
		for _, revision := range list {
			record := resources.RevisionRecord{}

			if val, ok := revision.Annotations[autoscaling.MinScaleAnnotationKey]; ok {
				i, err := strconv.ParseInt(val, 10, 32)
				if err == nil {
					record.MinScale = ptr.Int32(int32(i))
				}
			}

			if val, ok := revision.Annotations[autoscaling.MaxScaleAnnotationKey]; ok {
				i, err := strconv.ParseInt(val, 10, 32)
				if err == nil {
					record.MaxScale = ptr.Int32(int32(i))
				}
			}
			record.Name = revision.Name
			records[revision.Name] = record

		}
	}
	//else {
	//	return nil, fmt.Errorf("failed to get the revision: %w", err)
	//}

	so = resources.MakeServiceOrchestrator(service, route, records, logger, so)
	if create {
		so, err = e.client.ServingV1().ServiceOrchestrators(service.Namespace).Create(
			ctx, so, metav1.CreateOptions{})
		if err != nil {
			return so, err
		}
	} else {
		so, err = e.client.ServingV1().ServiceOrchestrators(service.Namespace).Update(ctx, so, metav1.UpdateOptions{})
		if err != nil {
			return so, err
		}
	}
	so, _ = e.calculateStageRevisionTarget(ctx, so)
	origin := so.DeepCopy()
	if equality.Semantic.DeepEqual(origin.Spec, so.Spec) {
		return so, nil
	}
	so, err = e.client.ServingV1().ServiceOrchestrators(service.Namespace).Update(ctx, so, metav1.UpdateOptions{})
	return so, err
}

func (e *progressiveRolloutExtension) reconcileServiceOrchestrator(ctx context.Context, service *v1.Service,
	route *v1.Route, so *v1.ServiceOrchestrator) (*v1.ServiceOrchestrator, error) {
	so1, err := e.createUpdateServiceOrchestrator(ctx, service, route, so, false)
	if err != nil {
		return so, err
	}
	if equality.Semantic.DeepEqual(so.Spec, so1.Spec) {
		return so, nil
	}
	return e.client.ServingV1().ServiceOrchestrators(service.Namespace).Update(ctx, so1, metav1.UpdateOptions{})
}

func (e *progressiveRolloutExtension) calculateStageRevisionTarget(ctx context.Context, so *v1.ServiceOrchestrator) (*v1.ServiceOrchestrator, error) {
	if so.Status.StageRevisionStatus == nil || len(so.Status.StageRevisionStatus) == 0 ||
		so.Spec.InitialRevisionStatus == nil || len(so.Spec.InitialRevisionStatus) == 0 {

		// There is no stage revision status, which indicates that no route is configured. We can directly set
		// the ultimate revision target as the current stage revision target.
		so.Spec.StageRevisionTarget = append([]v1.RevisionTarget{}, so.Spec.RevisionTarget...)

	} else {
		if len(so.Spec.InitialRevisionStatus) > 2 || len(so.Spec.RevisionTarget) > 1 {
			// If the initial revision status contains more than one revision, or the ultimate revision target contains
			// more than one revision, we will set the current stage target to the ultimate revision target.
			so.Spec.StageRevisionTarget = append([]v1.RevisionTarget{}, so.Spec.RevisionTarget...)
			//} else if len(so.Spec.InitialRevisionStatus) == 2 {
			//	// TODO this is a special case
			//	if *so.Spec.InitialRevisionStatus[0].Percent != int64(100) || *so.Spec.InitialRevisionStatus[0].Percent != int64(0) {
			//		so.Spec.StageRevisionTarget = append([]v1.RevisionTarget{}, so.Spec.RevisionTarget...)
			//	}

		} else {
			if so.Spec.StageRevisionTarget == nil {
				// If so.Spec.StageRevisionTarget is not empty, we need to calculate the stage revision target.
				so = e.updateStageRevisionSpec(so)
				return so, nil
			}
			// If the initial revision status and ultimate revision target both contains only one revision, we will
			// roll out the revision incrementally.
			// Check if stage revision status is ready or in progress
			if so.IsStageReady() {
				if so.IsReady() {
					// If the last stage has rolled out, nothing changes.
					return so, nil
				} else {
					// The current stage revision is complete. We need to calculate the next stage target.
					so = e.updateStageRevisionSpec(so)
				}
			} else if so.IsStageInProgress() {
				// Do nothing, because it is in progress to the current so.Spec.StageRevisionTarget
				// so.Spec.StageRevisionTarget is not empty.
				return so, nil
			}
		}
	}

	return so, nil
}

func (e *progressiveRolloutExtension) updateStageRevisionSpec(so *v1.ServiceOrchestrator) *v1.ServiceOrchestrator {
	if len(so.Status.StageRevisionStatus) > 2 || len(so.Spec.RevisionTarget) != 1 {
		return so
	}
	finalRevision := so.Spec.RevisionTarget[0].RevisionName

	startRevisionStatus := so.Status.StageRevisionStatus
	if so.Spec.StageRevisionTarget == nil {
		startRevisionStatus = so.Spec.InitialRevisionStatus
	}
	ratio := resources.OverSubRatio
	found := false
	index := -1
	if len(startRevisionStatus) == 2 {
		if startRevisionStatus[0].RevisionName == finalRevision {
			found = true
			index = 0
			//originIndex = 1
		}
		if startRevisionStatus[1].RevisionName == finalRevision {
			found = true
			index = 1
			//originIndex = 0
		}
		if !found {
			so.Spec.StageRevisionTarget = append([]v1.RevisionTarget{}, so.Spec.RevisionTarget...)
			return so
		}

		currentTraffic := *startRevisionStatus[index].Percent

		//	finalTraffic := *so.Spec.RevisionTarget[0].Percent

		pa, _ := e.podAutoscalerLister.PodAutoscalers(so.Namespace).Get(finalRevision)
		currentReplicas := *pa.Status.DesiredScale

		pa, _ = e.podAutoscalerLister.PodAutoscalers(so.Namespace).Get(finalRevision)
		targetReplicas := int32(32)
		if pa != nil {
			targetReplicas = *pa.Status.DesiredScale
		}
		if targetReplicas < 0 {
			targetReplicas = 0
		}

		min := startRevisionStatus[index].MinScale
		max := startRevisionStatus[index].MaxScale

		stageRevisionTarget := []v1.RevisionTarget{}
		if min == nil {
			if max == nil {
				if currentReplicas == 0 {
					// No traffic, set the stage revision target to final revision target.
					stageRevisionTarget = append([]v1.RevisionTarget{}, so.Spec.RevisionTarget...)
				} else {
					// Driven by traffic
					stageRevisionTarget = e.trafficDriven(startRevisionStatus, index, so.Spec.RevisionTarget, currentTraffic, so.Namespace, currentReplicas, targetReplicas, ratio)
				}

			} else {
				maxV := *max
				if currentReplicas == 0 {
					// No traffic, set the stage revision target to final revision target.
					stageRevisionTarget = append([]v1.RevisionTarget{}, so.Spec.RevisionTarget...)
				} else if currentReplicas < maxV {
					// Driven by traffic
					stageRevisionTarget = e.trafficDriven(startRevisionStatus, index, so.Spec.RevisionTarget, currentTraffic, so.Namespace, currentReplicas, targetReplicas, ratio)
				} else if currentReplicas == maxV {
					// Full load.
					stageRevisionTarget = e.fullLoad(startRevisionStatus, index, so.Spec.RevisionTarget, currentTraffic, so.Namespace, currentReplicas, targetReplicas, ratio)
				}
			}
		} else {
			if max == nil {
				minV := *min
				if currentReplicas == 0 {
					// No traffic, set the stage revision target to final revision target.
					stageRevisionTarget = append([]v1.RevisionTarget{}, so.Spec.RevisionTarget...)
				} else if currentReplicas <= minV {
					// Lowest load.
					stageRevisionTarget = e.lowestLoad(startRevisionStatus, index, so.Spec.RevisionTarget, currentTraffic, so.Namespace, currentReplicas, targetReplicas, ratio)

				} else if currentReplicas > minV {
					// Driven by traffic
					stageRevisionTarget = e.trafficDriven(startRevisionStatus, index, so.Spec.RevisionTarget, currentTraffic, so.Namespace, currentReplicas, targetReplicas, ratio)
				}

			} else {
				minV := *min
				maxV := *max
				if currentReplicas == 0 {
					// No traffic, set the stage revision target to final revision target.
					stageRevisionTarget = append([]v1.RevisionTarget{}, so.Spec.RevisionTarget...)
				} else if currentReplicas > minV && currentReplicas < maxV {
					// Driven by traffic
					stageRevisionTarget = e.trafficDriven(startRevisionStatus, index, so.Spec.RevisionTarget, currentTraffic, so.Namespace, currentReplicas, targetReplicas, ratio)
				} else if currentReplicas == maxV {
					// Full load.
					stageRevisionTarget = e.fullLoad(startRevisionStatus, index, so.Spec.RevisionTarget, currentTraffic, so.Namespace, currentReplicas, targetReplicas, ratio)
				} else if currentReplicas <= minV {
					// Lowest load.
					stageRevisionTarget = e.lowestLoad(startRevisionStatus, index, so.Spec.RevisionTarget, currentTraffic, so.Namespace, currentReplicas, targetReplicas, ratio)
				}

			}
		}

		so.Spec.StageRevisionTarget = stageRevisionTarget
		t := time.Now()
		so.Spec.StageTarget.TargetFinishTime.Inner = metav1.NewTime(t.Add(time.Minute * 2))
	}

	if len(startRevisionStatus) == 1 {
		if startRevisionStatus[0].RevisionName == finalRevision {
			so.Spec.StageRevisionTarget = so.Spec.RevisionTarget
			return so
		}

		min := startRevisionStatus[0].MinScale
		max := startRevisionStatus[0].MaxScale
		index = 0
		pa, _ := e.podAutoscalerLister.PodAutoscalers(so.Namespace).Get(startRevisionStatus[0].RevisionName)
		currentReplicas := *pa.Status.DesiredScale

		pa, _ = e.podAutoscalerLister.PodAutoscalers(so.Namespace).Get(finalRevision)

		targetReplicas := int32(32)
		if pa != nil {
			targetReplicas = *pa.Status.DesiredScale
		}
		if targetReplicas < 0 {
			targetReplicas = 0
		}

		currentTraffic := *startRevisionStatus[0].Percent

		//	finalTraffic := *so.Spec.RevisionTarget[0].Percent

		stageRevisionTarget := []v1.RevisionTarget{}
		if min == nil {
			if max == nil {
				if currentReplicas == 0 {
					// No traffic, set the stage revision target to final revision target.
					stageRevisionTarget = append([]v1.RevisionTarget{}, so.Spec.RevisionTarget...)
				} else {
					// Driven by traffic
					stageRevisionTarget = e.trafficDriven(startRevisionStatus, index, so.Spec.RevisionTarget, currentTraffic, so.Namespace, currentReplicas, targetReplicas, ratio)
				}

			} else {
				maxV := *max
				if currentReplicas == 0 {
					// No traffic, set the stage revision target to final revision target.
					stageRevisionTarget = append([]v1.RevisionTarget{}, so.Spec.RevisionTarget...)
				} else if currentReplicas < maxV {
					// Driven by traffic
					stageRevisionTarget = e.trafficDriven(startRevisionStatus, index, so.Spec.RevisionTarget, currentTraffic, so.Namespace, currentReplicas, targetReplicas, ratio)
				} else if currentReplicas == maxV {
					// Full load.
					stageRevisionTarget = e.fullLoad(startRevisionStatus, index, so.Spec.RevisionTarget, currentTraffic, so.Namespace, currentReplicas, targetReplicas, ratio)
				}
			}
		} else {
			if max == nil {
				minV := *min
				if currentReplicas == 0 {
					// No traffic, set the stage revision target to final revision target.
					stageRevisionTarget = append([]v1.RevisionTarget{}, so.Spec.RevisionTarget...)
				} else if currentReplicas <= minV {
					// Lowest load.
					stageRevisionTarget = e.lowestLoad(startRevisionStatus, index, so.Spec.RevisionTarget, currentTraffic, so.Namespace, currentReplicas, targetReplicas, ratio)

				} else if currentReplicas > minV {
					// Driven by traffic
					stageRevisionTarget = e.trafficDriven(startRevisionStatus, index, so.Spec.RevisionTarget, currentTraffic, so.Namespace, currentReplicas, targetReplicas, ratio)
				}

			} else {
				minV := *min
				maxV := *max
				if currentReplicas == 0 {
					// No traffic, set the stage revision target to final revision target.
					stageRevisionTarget = append([]v1.RevisionTarget{}, so.Spec.RevisionTarget...)
				} else if currentReplicas > minV && currentReplicas < maxV {
					// Driven by traffic
					stageRevisionTarget = e.trafficDriven(startRevisionStatus, index, so.Spec.RevisionTarget, currentTraffic, so.Namespace, currentReplicas, targetReplicas, ratio)
				} else if currentReplicas == maxV {
					// Full load.
					stageRevisionTarget = e.fullLoad(startRevisionStatus, index, so.Spec.RevisionTarget, currentTraffic, so.Namespace, currentReplicas, targetReplicas, ratio)
				} else if currentReplicas == minV {
					// Lowest load.
					stageRevisionTarget = e.lowestLoad(startRevisionStatus, index, so.Spec.RevisionTarget, currentTraffic, so.Namespace, currentReplicas, targetReplicas, ratio)
				}

			}
		}

		//stageRevisionTarget.
		so.Spec.StageRevisionTarget = stageRevisionTarget
		t := time.Now()

		so.Spec.StageTarget.TargetFinishTime.Inner = metav1.NewTime(t.Add(time.Minute * 2))
	}

	return so
}

func (e *progressiveRolloutExtension) trafficDriven(rt []v1.RevisionTarget, index int, rtF []v1.RevisionTarget,
	currentTraffic int64, namespace string, currentReplicas, targetReplicas int32, ratio int) []v1.RevisionTarget {
	return e.lowestLoad(rt, index, rtF, currentTraffic, namespace, currentReplicas, targetReplicas, ratio)
}

func (e *progressiveRolloutExtension) fullLoad(rt []v1.RevisionTarget, index int, rtF []v1.RevisionTarget,
	currentTraffic int64, namespace string, currentReplicas, targetReplicas int32, ratio int) []v1.RevisionTarget {

	return e.lowestLoad(rt, index, rtF, currentTraffic, namespace, currentReplicas, targetReplicas, ratio)
}

func getReplicasTraffic(percent int64, currentReplicas int32, ratio int) (int32, int64) {
	stageReplicas := math.Ceil(float64(int(currentReplicas)) * float64(ratio) / float64((int(percent))))

	stageTrafficDelta := math.Ceil(stageReplicas * float64((int(percent))) / float64(int(currentReplicas)))

	return int32(stageReplicas), int64(stageTrafficDelta)
}

func (e *progressiveRolloutExtension) lowestLoad(rt []v1.RevisionTarget, index int, rtF []v1.RevisionTarget,
	currentTraffic int64, namespace string, currentReplicas, targetReplicas int32, ratio int) []v1.RevisionTarget {
	stageReplicasInt, stageTrafficDeltaInt := getReplicasTraffic(*rt[index].Percent, currentReplicas, ratio)
	var stageRevisionTarget []v1.RevisionTarget
	if len(rt) == 1 {
		stageRevisionTarget = make([]v1.RevisionTarget, 2, 2)
		if stageTrafficDeltaInt >= 100 {
			stageRevisionTarget = append(rtF, []v1.RevisionTarget{}...)
			target := v1.RevisionTarget{}
			target.RevisionName = rt[0].RevisionName
			target.MaxScale = rt[0].MaxScale
			target.MinScale = rt[0].MinScale
			target.Direction = "down"
			target.Percent = ptr.Int64(0)
			target.TargetReplicas = ptr.Int32(0)
			stageRevisionTarget = append(stageRevisionTarget, target)
			return stageRevisionTarget
		}

		targetNewRollout := v1.RevisionTarget{}
		targetNewRollout.RevisionName = rtF[0].RevisionName
		targetNewRollout.LatestRevision = ptr.Bool(true)
		targetNewRollout.MinScale = rtF[0].MinScale
		targetNewRollout.MaxScale = rtF[0].MaxScale
		targetNewRollout.Direction = "up"
		targetNewRollout.TargetReplicas = ptr.Int32(stageReplicasInt)
		targetNewRollout.Percent = ptr.Int64(stageTrafficDeltaInt)
		stageRevisionTarget[1] = targetNewRollout

		target := v1.RevisionTarget{}
		target.RevisionName = rt[0].RevisionName
		target.LatestRevision = ptr.Bool(false)
		target.MinScale = rt[0].MinScale
		target.MaxScale = rt[0].MaxScale
		target.Direction = "down"
		target.TargetReplicas = ptr.Int32(currentReplicas - stageReplicasInt)
		target.Percent = ptr.Int64(currentTraffic - stageTrafficDeltaInt)
		stageRevisionTarget[0] = target

	} else if len(rt) == 2 {
		stageRevisionTarget = make([]v1.RevisionTarget, 0, 2)
		for i, r := range rt {
			if i == index {
				nu := *r.Percent + stageTrafficDeltaInt
				if nu >= 100 {
					fmt.Println("up over 100")
					stageRevisionTarget = append(stageRevisionTarget, rtF...)
					//target := v1.RevisionTarget{}
					//target.TargetReplicas = ptr.Int32(0)
					//stageRevisionTarget = append(stageRevisionTarget, target)
					//return stageRevisionTarget
					fmt.Println(stageRevisionTarget)
				} else {
					target := v1.RevisionTarget{}
					target.RevisionName = r.RevisionName
					target.LatestRevision = ptr.Bool(true)
					target.MinScale = r.MinScale
					target.MaxScale = r.MaxScale
					target.Direction = "up"
					target.TargetReplicas = ptr.Int32(targetReplicas + stageReplicasInt)
					target.Percent = ptr.Int64(*r.Percent + stageTrafficDeltaInt)
					stageRevisionTarget = append(stageRevisionTarget, target)
				}

			} else {
				pa, _ := e.podAutoscalerLister.PodAutoscalers(namespace).Get(r.RevisionName)
				oldReplicas := int32(0)
				if pa != nil {
					oldReplicas = *pa.Status.DesiredScale
				}
				if oldReplicas < 0 {
					oldReplicas = 0
				}

				if *r.Percent-stageTrafficDeltaInt <= 0 {
					target := v1.RevisionTarget{}
					target.RevisionName = r.RevisionName
					target.LatestRevision = ptr.Bool(false)
					target.MinScale = r.MinScale
					target.MaxScale = r.MaxScale
					target.Direction = "down"
					target.TargetReplicas = ptr.Int32(0)
					target.Percent = ptr.Int64(0)
					stageRevisionTarget = append(stageRevisionTarget, target)
					fmt.Println("down below 0")
					fmt.Println(stageRevisionTarget)
				} else {
					target := v1.RevisionTarget{}
					target.RevisionName = r.RevisionName
					target.LatestRevision = ptr.Bool(false)
					target.MinScale = r.MinScale
					target.MaxScale = r.MaxScale
					target.Direction = "down"
					if oldReplicas-stageReplicasInt <= 0 {
						target.TargetReplicas = r.TargetReplicas
					} else {
						target.TargetReplicas = ptr.Int32(oldReplicas - stageReplicasInt)
					}
					if *r.Percent-stageTrafficDeltaInt <= 0 {
						target.Percent = ptr.Int64(0)
					} else {
						target.Percent = ptr.Int64(*r.Percent - stageTrafficDeltaInt)
					}

					stageRevisionTarget = append(stageRevisionTarget, target)
					fmt.Println("down not below 0")
					fmt.Println(stageRevisionTarget)
				}

			}
		}

	}

	return stageRevisionTarget
}

func makeRevC(rev *v1.Revision, spa *v1.StagePodAutoscaler) *v1.Revision {
	if spa.Spec.MinScale != nil {
		rev.Annotations[autoscaling.MinScaleAnnotationKey] = fmt.Sprintf("%v", *spa.Spec.MinScale)
	}

	if spa.Spec.MaxScale != nil {
		rev.Annotations[autoscaling.MaxScaleAnnotationKey] = fmt.Sprintf("%v", *spa.Spec.MaxScale)
	}
	return rev
}
