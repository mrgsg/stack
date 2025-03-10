package task

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"go.uber.org/dig"

	"github.com/google/uuid"

	"github.com/formancehq/payments/internal/app/metrics"
	"github.com/formancehq/payments/internal/app/storage"

	"github.com/formancehq/payments/internal/app/models"

	"github.com/formancehq/stack/libs/go-libs/logging"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var (
	ErrAlreadyScheduled = errors.New("already scheduled")
	ErrUnableToResolve  = errors.New("unable to resolve task")
)

type Scheduler interface {
	Schedule(ctx context.Context, p models.TaskDescriptor, options models.TaskSchedulerOptions) error
}

type taskHolder struct {
	descriptor models.TaskDescriptor
	cancel     func()
	logger     logging.Logger
	stopChan   StopChan
}

type ContainerCreateFunc func(ctx context.Context, descriptor models.TaskDescriptor, taskID uuid.UUID) (*dig.Container, error)

type DefaultTaskScheduler struct {
	provider         models.ConnectorProvider
	store            Repository
	metricsRegistry  metrics.MetricsRegistry
	containerFactory ContainerCreateFunc
	tasks            map[string]*taskHolder
	mu               sync.Mutex
	maxTasks         int
	resolver         Resolver
	stopped          bool
}

func (s *DefaultTaskScheduler) ListTasks(ctx context.Context, pagination storage.Paginator) ([]models.Task, storage.PaginationDetails, error) {
	return s.store.ListTasks(ctx, s.provider, pagination)
}

func (s *DefaultTaskScheduler) ReadTask(ctx context.Context, taskID uuid.UUID) (*models.Task, error) {
	return s.store.GetTask(ctx, taskID)
}

func (s *DefaultTaskScheduler) ReadTaskByDescriptor(ctx context.Context, descriptor models.TaskDescriptor) (*models.Task, error) {
	taskDescriptor, err := json.Marshal(descriptor)
	if err != nil {
		return nil, err
	}

	return s.store.GetTaskByDescriptor(ctx, s.provider, taskDescriptor)
}

// Schedule schedules a task to be executed.
// Schedule waits for:
//   - Context to be done
//   - Task creation if the scheduler option is not equal to OPTIONS_RUN_NOW_SYNC
//   - Task termination if the scheduler option is equal to OPTIONS_RUN_NOW_SYNC
func (s *DefaultTaskScheduler) Schedule(ctx context.Context, descriptor models.TaskDescriptor, options models.TaskSchedulerOptions) error {
	select {
	case err := <-s.schedule(ctx, descriptor, options):
		return err
	case <-ctx.Done():
		return nil
	}
}

// schedule schedules a task to be executed.
// It returns an error chan that will be closed when the task is terminated if
// the scheduler option is equal to OPTIONS_RUN_NOW_SYNC. Otherwise, it will
// return an error chan that will be closed immediately after task creation.
func (s *DefaultTaskScheduler) schedule(ctx context.Context, descriptor models.TaskDescriptor, options models.TaskSchedulerOptions) <-chan error {
	s.mu.Lock()
	defer s.mu.Unlock()

	returnErrorFunc := func(err error) <-chan error {
		errChan := make(chan error, 1)
		if err != nil {
			errChan <- err
		}
		close(errChan)
		return errChan
	}

	taskID, err := descriptor.EncodeToString()
	if err != nil {
		return returnErrorFunc(err)
	}

	if _, ok := s.tasks[taskID]; ok {
		return returnErrorFunc(ErrAlreadyScheduled)
	}

	switch options.RestartOption {
	case models.OPTIONS_RESTART_NEVER:
		_, err := s.ReadTaskByDescriptor(ctx, descriptor)
		if err == nil {
			return returnErrorFunc(nil)
		}
	case models.OPTIONS_RESTART_IF_NOT_ACTIVE:
		task, err := s.ReadTaskByDescriptor(ctx, descriptor)
		if err == nil && task.Status == models.TaskStatusActive {
			return nil
		}
	case models.OPTIONS_RESTART_ALWAYS:
		// Do nothing
	}

	if s.maxTasks != 0 && len(s.tasks) >= s.maxTasks || s.stopped {
		err := s.stackTask(ctx, descriptor)
		if err != nil {
			return returnErrorFunc(errors.Wrap(err, "stacking task"))
		}

		return returnErrorFunc(nil)
	}

	errChan := s.startTask(ctx, descriptor, options)

	return errChan
}

func (s *DefaultTaskScheduler) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()

	s.logger(ctx).Infof("Stopping scheduler...")

	for name, task := range s.tasks {
		task.logger.Debugf("Stopping task")

		if task.stopChan != nil {
			errCh := make(chan struct{})
			task.stopChan <- errCh
			select {
			case <-errCh:
			case <-time.After(time.Second): // TODO: Make configurable
				task.logger.Debugf("Stopping using stop chan timeout, canceling context")
				task.cancel()
			}
		} else {
			task.cancel()
		}

		delete(s.tasks, name)
	}

	return nil
}

func (s *DefaultTaskScheduler) Restore(ctx context.Context) error {
	tasks, err := s.store.ListTasksByStatus(ctx, s.provider, models.TaskStatusActive)
	if err != nil {
		return err
	}

	for _, task := range tasks {
		if task.SchedulerOptions.Restart {
			task.SchedulerOptions.RestartOption = models.OPTIONS_RESTART_ALWAYS
		}

		errChan := s.startTask(ctx, task.GetDescriptor(), task.SchedulerOptions)
		select {
		case err := <-errChan:
			if err != nil {
				s.logger(ctx).Errorf("Unable to restore task %s: %s", task.ID, err)
			}
		case <-ctx.Done():
		}
	}

	return nil
}

func (s *DefaultTaskScheduler) registerTaskError(ctx context.Context, holder *taskHolder, taskErr any) {
	var taskError string

	switch v := taskErr.(type) {
	case error:
		taskError = v.Error()
	default:
		taskError = fmt.Sprintf("%s", v)
	}

	holder.logger.Errorf("Task terminated with error: %s", taskErr)

	err := s.store.UpdateTaskStatus(ctx, s.provider, holder.descriptor, models.TaskStatusFailed, taskError)
	if err != nil {
		holder.logger.Error("Error updating task status: %s", taskError)
	}
}

func (s *DefaultTaskScheduler) deleteTask(ctx context.Context, holder *taskHolder) {
	s.mu.Lock()
	defer s.mu.Unlock()

	taskID, err := holder.descriptor.EncodeToString()
	if err != nil {
		holder.logger.Errorf("Error encoding task descriptor: %s", err)

		return
	}

	delete(s.tasks, taskID)

	if s.stopped {
		return
	}

	oldestPendingTask, err := s.store.ReadOldestPendingTask(ctx, s.provider)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return
		}

		logging.FromContext(ctx).Error(err)

		return
	}

	p := s.resolver.Resolve(oldestPendingTask.GetDescriptor())
	if p == nil {
		logging.FromContext(ctx).Errorf("unable to resolve task")

		return
	}

	errChan := s.startTask(ctx, oldestPendingTask.GetDescriptor(), models.TaskSchedulerOptions{
		ScheduleOption: models.OPTIONS_RUN_NOW,
	})
	select {
	case err, ok := <-errChan:
		if !ok {
			return
		}
		if err != nil {
			logging.FromContext(ctx).Error(err)
		}
	case <-ctx.Done():
		return
	}
}

type StopChan chan chan struct{}

func (s *DefaultTaskScheduler) startTask(ctx context.Context, descriptor models.TaskDescriptor, options models.TaskSchedulerOptions) <-chan error {
	errChan := make(chan error, 1)

	task, err := s.store.FindAndUpsertTask(ctx, s.provider, descriptor,
		models.TaskStatusActive, options, "")
	if err != nil {
		errChan <- errors.Wrap(err, "finding task and update")
		close(errChan)
		return errChan
	}

	logger := s.logger(ctx).WithFields(map[string]interface{}{
		"task-id": task.ID,
	})

	taskResolver := s.resolver.Resolve(task.GetDescriptor())
	if taskResolver == nil {
		errChan <- ErrUnableToResolve
		close(errChan)
		return errChan
	}

	ctx, cancel := context.WithCancel(ctx)
	ctx, span := otel.Tracer("com.formance.payments").Start(ctx, "Task", trace.WithAttributes(
		attribute.Stringer("id", task.ID),
		attribute.Stringer("connector", s.provider),
	))

	holder := &taskHolder{
		cancel:     cancel,
		logger:     logger,
		descriptor: descriptor,
	}

	container, err := s.containerFactory(ctx, descriptor, task.ID)
	if err != nil {
		// TODO: Handle error
		panic(err)
	}

	err = container.Provide(func() context.Context {
		return ctx
	})
	if err != nil {
		panic(err)
	}

	err = container.Provide(func() Scheduler {
		return s
	})
	if err != nil {
		panic(err)
	}

	err = container.Provide(func() StopChan {
		s.mu.Lock()
		defer s.mu.Unlock()

		holder.stopChan = make(StopChan, 1)

		return holder.stopChan
	})
	if err != nil {
		panic(err)
	}

	err = container.Provide(func() logging.Logger {
		return logger
	})
	if err != nil {
		panic(err)
	}

	err = container.Provide(func() metrics.MetricsRegistry {
		return s.metricsRegistry
	})
	if err != nil {
		panic(err)
	}

	err = container.Provide(func() StateResolver {
		return StateResolverFn(func(ctx context.Context, v any) error {
			if task.State == nil || len(task.State) == 0 {
				return nil
			}

			return json.Unmarshal(task.State, v)
		})
	})
	if err != nil {
		panic(err)
	}

	taskID, err := holder.descriptor.EncodeToString()
	if err != nil {
		errChan <- err
		close(errChan)
		return errChan
	}

	s.tasks[taskID] = holder

	sendError := false
	switch options.ScheduleOption {
	case models.OPTIONS_RUN_NOW_SYNC:
		sendError = true
		fallthrough
	case models.OPTIONS_RUN_NOW:
		options.Duration = 0
		fallthrough
	case models.OPTIONS_RUN_IN_DURATION:
		go func() {
			if options.Duration > 0 {
				logger.Infof("Waiting %s before starting task...", options.Duration)
				time.Sleep(options.Duration)
			}

			logger.Infof("Starting task...")

			defer func() {
				defer span.End()
				defer s.deleteTask(ctx, holder)

				if sendError {
					defer close(errChan)
				}

				if e := recover(); e != nil {
					s.registerTaskError(ctx, holder, e)
					debug.PrintStack()

					if sendError {
						switch v := e.(type) {
						case error:
							errChan <- v
						default:
							errChan <- fmt.Errorf("%s", v)
						}
					}
					return
				}
			}()

			err = container.Invoke(taskResolver)
			if err != nil {
				s.registerTaskError(ctx, holder, err)

				if sendError {
					errChan <- err
					return
				}

				return
			}

			logger.Infof("Task terminated with success")

			err = s.store.UpdateTaskStatus(ctx, s.provider, descriptor, models.TaskStatusTerminated, "")
			if err != nil {
				logger.Error("Error updating task status: %s", err)
				if sendError {
					errChan <- err
				}
			}
		}()
	case models.OPTIONS_RUN_INDEFINITELY:
		go func() {
			defer func() {
				defer span.End()
				defer s.deleteTask(ctx, holder)

				if e := recover(); e != nil {
					s.registerTaskError(ctx, holder, e)
					debug.PrintStack()

					return
				}
			}()

			// launch it once before starting the ticker
			err = container.Invoke(taskResolver)
			if err != nil {
				s.registerTaskError(ctx, holder, err)

				return
			}

			logger.Infof("Starting task...")
			ticker := time.NewTicker(options.Duration)
			for {
				select {
				case ch := <-holder.stopChan:
					logger.Infof("Stopping task...")
					close(ch)
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					logger.Infof("Polling trigger, running task...")
					err = container.Invoke(taskResolver)
					if err != nil {
						s.registerTaskError(ctx, holder, err)

						return
					}
				}
			}

		}()
	}

	if !sendError {
		close(errChan)
	}

	return errChan
}

func (s *DefaultTaskScheduler) stackTask(ctx context.Context, descriptor models.TaskDescriptor) error {
	s.logger(ctx).WithFields(map[string]interface{}{
		"descriptor": string(descriptor),
	}).Infof("Stacking task")

	return s.store.UpdateTaskStatus(ctx, s.provider, descriptor, models.TaskStatusPending, "")
}

func (s *DefaultTaskScheduler) logger(ctx context.Context) logging.Logger {
	return logging.FromContext(ctx).WithFields(map[string]any{
		"component": "scheduler",
		"provider":  s.provider,
	})
}

var _ Scheduler = &DefaultTaskScheduler{}

func NewDefaultScheduler(
	provider models.ConnectorProvider,
	store Repository,
	containerFactory ContainerCreateFunc,
	resolver Resolver,
	metricsRegistry metrics.MetricsRegistry,
	maxTasks int,
) *DefaultTaskScheduler {
	return &DefaultTaskScheduler{
		provider:         provider,
		store:            store,
		metricsRegistry:  metricsRegistry,
		tasks:            map[string]*taskHolder{},
		containerFactory: containerFactory,
		maxTasks:         maxTasks,
		resolver:         resolver,
	}
}
