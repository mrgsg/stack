package modules

import (
	"context"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/pkg/errors"

	"github.com/formancehq/operator/apis/stack/v1beta3"
	"github.com/formancehq/stack/libs/go-libs/collectionutils"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type Services []*Service

func (services Services) Len() int {
	return len(services)
}

func (services Services) Less(i, j int) bool {
	return strings.Compare(services[i].Name, services[j].Name) < 0
}

func (services Services) Swap(i, j int) {
	services[i], services[j] = services[j], services[i]
}

type Cron struct {
	Container Container
	Schedule  string
	Suspend   bool
}

type DatabaseMigration struct {
	Shutdown      bool
	Command       []string
	AdditionalEnv func(config ReconciliationConfig) []EnvVar
}

type Version struct {
	DatabaseMigration *DatabaseMigration
	Services          func(cfg ReconciliationConfig) Services
	Cron              func(cfg ReconciliationConfig) []Cron
	PreUpgrade        func(ctx context.Context, cfg ReconciliationConfig) error
	PostUpgrade       func(ctx context.Context, cfg ReconciliationConfig) error
}

type Module interface {
	Name() string
	Versions() map[string]Version
}

type DependsOnAwareModule interface {
	Module
	DependsOn() []Module
}

type PostgresAwareModule interface {
	Module
	Postgres(cfg ReconciliationConfig) v1beta3.PostgresConfig
}

var modules = make([]Module, 0)

func Register(newModules ...Module) {
	modules = append(modules, newModules...)
}

func Get(name string) Module {
	for _, module := range modules {
		if module.Name() == name {
			return module
		}
	}
	return nil
}

func sortedVersions(module Module) []string {
	versionsKeys := collectionutils.Keys(module.Versions())
	sort.Strings(versionsKeys)

	return versionsKeys
}

func falseIfError(err error) (bool, error) {
	if err != nil {
		return false, err
	}
	return true, nil
}

type moduleReconciler struct {
	*StackReconciler
	module Module
}

func (r *moduleReconciler) installModule(ctx context.Context, registeredModules RegisteredModules) (bool, error) {

	logger := log.FromContext(ctx)
	logger.Info(fmt.Sprintf("Installing module %s", r.module.Name()))

	registeredModule := RegisteredModule{
		Module:   r.module,
		Services: map[string]RegisteredService{},
	}
	registeredModules[r.module.Name()] = registeredModule

	var (
		postgresConfig v1beta3.PostgresConfig
		err            error
	)
	pam, ok := r.module.(PostgresAwareModule)
	if ok {
		postgresConfig = pam.Postgres(r.ReconciliationConfig)
		ok, err = r.createDatabase(ctx, postgresConfig)
		if err != nil {
			return false, err
		}
		if !ok {
			logger.Info("Waiting for database to be created", "module", pam.Name())
			return false, nil
		}
	}

	var (
		chosenVersion      Version
		chosenVersionLabel string
	)
	for _, version := range sortedVersions(r.module) {
		if !r.Versions.IsHigherOrEqual(r.module.Name(), version) {
			break
		}
		chosenVersion = r.module.Versions()[version]
		chosenVersionLabel = version
		if chosenVersion.PreUpgrade == nil {
			continue
		}

		ready, err := r.runPreUpgradeMigration(ctx, r.module, version)
		if err != nil {
			return false, err
		}
		if !ready {
			return false, nil
		}
	}

	if chosenVersion.DatabaseMigration != nil {
		logger.Info("Start database migration process", "pod", r.module.Name())
		databaseMigrated, err := r.runDatabaseMigration(ctx, chosenVersionLabel, *chosenVersion.DatabaseMigration, postgresConfig)
		if err != nil {
			return false, err
		}
		if !databaseMigrated {
			logger.Info("Mark module as not ready since the database is not up to date")
			return false, nil
		}
	}

	services := chosenVersion.Services(r.ReconciliationConfig)
	sort.Stable(services)

	me := &serviceErrors{}
	for _, service := range services {
		serviceName := r.module.Name()
		if service.Name != "" {
			serviceName = serviceName + "-" + service.Name
		}

		serviceReconciler := newServiceReconciler(r, *service, serviceName)
		err := serviceReconciler.reconcile(ctx, ServiceInstallConfiguration{
			ReconciliationConfig: r.ReconciliationConfig,
			RegisteredModules:    registeredModules,
			PostgresConfig:       &postgresConfig,
		})
		if err != nil {
			me.setError(serviceName, err)
		}

		registeredModule.Services[serviceName] = RegisteredService{
			Port:    serviceReconciler.usedPort,
			Service: *service,
		}
	}
	if len(me.errors) > 0 {
		return false, me
	}

	return true, nil
}

func (r *moduleReconciler) finalizeModule(ctx context.Context, module Module) (bool, error) {
	versions := module.Versions()

	var selectedVersion Version
	for _, version := range sortedVersions(module) {
		if !r.Versions.IsHigherOrEqual(module.Name(), version) {
			break
		}
		selectedVersion = versions[version]
		if selectedVersion.PostUpgrade == nil {
			continue
		}

		migration := &v1beta3.Migration{}
		migrationName := fmt.Sprintf("%s-%s-post-upgrade", module.Name(), version)
		if err := r.namespacedResourceDeployer.client.Get(ctx, types.NamespacedName{
			Namespace: r.Stack.Name,
			Name:      migrationName,
		}, migration); err != nil {
			if !apierrors.IsNotFound(err) {
				return false, err
			}
			_, err := r.namespacedResourceDeployer.Migrations().CreateOrUpdate(ctx, migrationName, func(t *v1beta3.Migration) {
				t.Spec = v1beta3.MigrationSpec{
					Configuration:   r.Configuration.Name,
					Module:          module.Name(),
					TargetedVersion: version,
					Version:         r.Versions.Name,
					PostUpgrade:     true,
				}
			})
			if err != nil {
				return false, err
			}
			log.FromContext(ctx).Info("Mark module as not not completed as we have just created the object",
				"module", r.module.Name(), "migration", migrationName)
			return false, nil
		}
		if !migration.Status.Terminated {
			log.FromContext(ctx).Info("Mark module as not not completed since migration is not terminated",
				"module", r.module.Name(), "migration", migrationName)
			return false, nil
		}
	}

	if selectedVersion.Cron != nil {
		for _, cron := range selectedVersion.Cron(r.ReconciliationConfig) {
			_, err := r.namespacedResourceDeployer.CronJobs().CreateOrUpdate(ctx, cron.Container.Name, func(t *batchv1.CronJob) {
				t.Spec = batchv1.CronJobSpec{
					Suspend:  &cron.Suspend,
					Schedule: cron.Schedule,
					JobTemplate: batchv1.JobTemplateSpec{
						Spec: batchv1.JobSpec{
							Template: corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									RestartPolicy: corev1.RestartPolicyNever,
									Containers: []corev1.Container{{
										Name:    cron.Container.Name,
										Image:   cron.Container.Image,
										Command: cron.Container.Command,
										Args:    cron.Container.Args,
										Env:     cron.Container.Env.ToCoreEnv(),
									}},
								},
							},
						},
					},
				}
			})
			if err != nil {
				return false, err
			}
		}
	}

	return true, nil
}

func (r *moduleReconciler) createDatabase(ctx context.Context, postgresConfig v1beta3.PostgresConfig) (bool, error) {
	dbName := r.Stack.GetServiceName(r.module.Name())
	// PG does not support 'CREATE IF NOT EXISTS ' construct, emulate it with the above query
	createDBCommand := `echo SELECT \'CREATE DATABASE \"${POSTGRES_DATABASE}\"\' WHERE NOT EXISTS \(SELECT FROM pg_database WHERE datname = \'${POSTGRES_DATABASE}\'\)\\gexec | psql -h ${POSTGRES_HOST} -p ${POSTGRES_PORT} -U ${POSTGRES_USERNAME}`
	if postgresConfig.DisableSSLMode {
		createDBCommand += ` "sslmode=disable"`
	}
	return r.jobMustSucceed(ctx, fmt.Sprintf("%s-create-database", r.module.Name()), nil,
		func(t *batchv1.Job) {
			t.Spec = batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyOnFailure,
						Containers: []corev1.Container{{
							Name:  "create-database",
							Image: "postgres:15-alpine",
							Args:  []string{"sh", "-c", createDBCommand},
							// There is only one service which use prefixed env var : ledger v1
							// Since the ledger v1 auto handle migrations, we don't care about passing a prefix
							Env: DefaultPostgresEnvVarsWithPrefix(postgresConfig, dbName, "").
								// psql use PGPASSWORD env var
								Append(Env("PGPASSWORD", "$(POSTGRES_PASSWORD)")).
								ToCoreEnv(),
						}},
					},
				},
			}
		})
}

func (r *moduleReconciler) runPreUpgradeMigration(ctx context.Context, module Module, version string) (bool, error) {
	migration := &v1beta3.Migration{}
	migrationName := fmt.Sprintf("%s-%s-pre-upgrade", module.Name(), version)
	if err := r.namespacedResourceDeployer.client.Get(ctx, types.NamespacedName{
		Namespace: r.Stack.Name,
		Name:      migrationName,
	}, migration); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, err
		}
		_, err := r.namespacedResourceDeployer.Migrations().CreateOrUpdate(ctx, migrationName, func(t *v1beta3.Migration) {
			t.Spec = v1beta3.MigrationSpec{
				Configuration:   r.Configuration.Name,
				Module:          module.Name(),
				TargetedVersion: version,
				Version:         r.Versions.Name,
			}
		})
		return false, err
	}

	return migration.Status.Terminated, nil
}

func (r *moduleReconciler) jobMustSucceed(ctx context.Context, jobName string, preRun func() error, modifier func(t *batchv1.Job)) (bool, error) {

	logger := log.FromContext(ctx)
	job := &batchv1.Job{}
	if err := r.namespacedResourceDeployer.client.Get(ctx, types.NamespacedName{
		Namespace: r.Stack.Name,
		Name:      jobName,
	}, job); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, err
		}

		logger.Info("Job not found", "pod", r.module.Name())
		if preRun != nil {
			if err := preRun(); err != nil {
				return false, err
			}
		}

		_, err := r.namespacedResourceDeployer.Jobs().CreateOrUpdate(ctx, jobName, modifier)
		if err != nil {
			return false, err
		}
		return false, nil
	}

	logger.Info(fmt.Sprintf("Job found, succeded: %d", job.Status.Succeeded))

	return job.Status.Succeeded > 0, nil
}

func (r *moduleReconciler) runDatabaseMigration(ctx context.Context, version string, migration DatabaseMigration, postgresConfig v1beta3.PostgresConfig) (bool, error) {
	logger := log.FromContext(ctx)
	return r.jobMustSucceed(ctx, fmt.Sprintf("%s-%s-database-migration", r.module.Name(), version),
		func() error {
			if migration.Shutdown {
				logger.Info("Stop module reconciliation as required by upgrade", "module", r.module.Name())
				// todo: store old replicas value
				if scaledDown, err := r.podDeployer.shutdown(ctx, r.module.Name()); err != nil {
					return errors.Wrap(err, "stopping pod")
				} else if !scaledDown {
					logger.Info("Stop reconciliation as pod needs to be scaled down", "pod", r.module.Name())
					return nil
				}
			}
			return nil
		},
		func(t *batchv1.Job) {
			args := migration.Command
			if len(args) == 0 {
				args = []string{"migrate"}
			}
			env := DefaultPostgresEnvVarsWithPrefix(postgresConfig, r.Stack.GetServiceName(r.module.Name()), "")
			if migration.AdditionalEnv != nil {
				env = env.Append(migration.AdditionalEnv(r.ReconciliationConfig)...)
			}
			t.Spec = batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyOnFailure,
						Containers: []corev1.Container{{
							Name:  "migrate",
							Image: GetImage(r.module.Name(), r.Versions.GetVersion(r.module.Name())),
							Args:  args,
							// There is only one service which use prefixed env var : ledger v1
							// Since the ledger v1 auto handle migrations, we don't care about passing a prefix
							Env: env.ToCoreEnv(),
						}},
					},
				},
			}
		})
}

func newModuleReconciler(stackReconciler *StackReconciler, module Module) *moduleReconciler {
	return &moduleReconciler{
		StackReconciler: stackReconciler,
		module:          module,
	}
}

func ensureDeploymentSync(ctx context.Context, deployment appsv1.Deployment) (bool, error) {
	logger := log.FromContext(ctx)
	if deployment.Status.ObservedGeneration != deployment.Generation {
		logger.Info(fmt.Sprintf("Stop reconciliation as deployment '%s' is not ready (generation not matching, generation: %d, observed: %d)",
			deployment.Name, deployment.Generation, deployment.Status.ObservedGeneration))
		return false, nil
	}
	var moreRecentCondition appsv1.DeploymentCondition
	for _, condition := range deployment.Status.Conditions {
		if moreRecentCondition.Type == "" || condition.LastTransitionTime.After(moreRecentCondition.LastTransitionTime.Time) {
			moreRecentCondition = condition
		}
	}
	if moreRecentCondition.Type != appsv1.DeploymentAvailable {
		logger.Info(fmt.Sprintf("Stop reconciliation as deployment '%s' is not ready (last condition must be '%s', found '%s')", deployment.Name, appsv1.DeploymentAvailable, moreRecentCondition.Type))
		return false, nil
	}
	if moreRecentCondition.Status != "True" {
		logger.Info(fmt.Sprintf("Stop reconciliation as deployment '%s' is not ready ('%s' condition should be 'true')", deployment.Name, appsv1.DeploymentAvailable))
		return false, nil
	}
	return true, nil
}
