package requeue

import (
	"context"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	RequeueDuration      = 60 * time.Second
	ErrorRequeueDuration = 30 * time.Second
)

func Always(ctx context.Context, err error) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	if err != nil {
		log.Error(err, "error during reconciliation")
		return requeueAfter(ErrorRequeueDuration), err
	}
	log.Info("reconciliation finished")
	return requeueAfter(RequeueDuration), nil
}

func OnError(ctx context.Context, err error) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	if err != nil {
		log.Error(err, "error during reconciliation")
		return requeueAfter(ErrorRequeueDuration), err
	}
	log.Info("reconciliation finished")
	return ctrl.Result{}, nil
}

func Never(ctx context.Context, err error) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	if err != nil {
		log.Error(err, "error during reconciliation")
	} else {
		log.Info("reconciliation finished")
	}
	return ctrl.Result{}, err
}

func requeueAfter(duration time.Duration) ctrl.Result {
	return ctrl.Result{RequeueAfter: duration}
}