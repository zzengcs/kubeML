package train

import (
	"fmt"
	"github.com/diegostock12/kubeml/ml/pkg/api"
	"github.com/diegostock12/kubeml/ml/pkg/model"
	psClient "github.com/diegostock12/kubeml/ml/pkg/ps/client"
	schedulerClient "github.com/diegostock12/kubeml/ml/pkg/scheduler/client"
	"github.com/diegostock12/kubeml/ml/pkg/util"
	"github.com/gomodule/redigo/redis"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"sync"
	"sync/atomic"
	"time"
)

// TrainJob is each of the workers launched by the parameter server.
// The worker is responsible from managing the reference model, saving the
// intermediate accuracy/validation results in the history, and requesting/receiving
// new scheduling responses from the scheduler
type TrainJob struct {
	logger *zap.Logger

	// clients for other components
	scheduler *schedulerClient.Client
	ps        *psClient.Client
	redisPool *redis.Pool //goroutines will fetch new connections from this pool to update the model in parallel

	// Training-specific resources
	history   api.JobHistory
	task      *api.TrainTask
	jobId     string
	epoch     int
	model     *model.Model
	optimizer model.ParallelSGD

	// options of the trainjob
	parallelism   int
	static        bool
	validateEvery int
	K             int
	goalAccuracy  float64 // validation accuracy that marks the stop moment

	// channel to receive updates from the scheduler
	// through the api
	schedulerCh chan *api.JobState

	// this channel needs to be buffered to prevent deadlock, if the validation
	// reaches the accuracy in the final validation outside of the loop,
	// it will try to reach the loop by sending to the channel, but the main
	// loop will no longer be waiting on the other side, but on the waitgroup, causing a deadlock
	// in this way the validation function can finish and return
	accuracyCh      chan struct{}
	accuracyReached bool

	// function synchronization, waitgroup
	// and index to track functions during an iteration
	wgIteration   *sync.WaitGroup
	finishedFuncs int64
	startMerger   chan chan error
	finishCh      chan *finishNotification
	merged        chan struct{}

	// keep track of the start time to compute stats
	startTime time.Time

	stopChan chan struct{}
	// exitErr holds the error that caused the job to quit
	// it is sent to the Ps along the finish signal so it can be
	// reported
	exitErr error
}

// NewTrainJob Creates a new TrainJob that will take care of a specific train request
func NewTrainJob(
	logger *zap.Logger,
	task *api.TrainTask,
	schedulerCh chan *api.JobState,
	client *schedulerClient.Client) *TrainJob {

	logger.Info("Creating new train job")

	job := &TrainJob{
		logger:      logger.Named(fmt.Sprintf("trainJob-%s", task.Job.JobId)),
		scheduler:   client,
		jobId:       task.Job.JobId,
		schedulerCh: schedulerCh,
		redisPool:   util.GetRedisConnectionPool(),
		history:     api.JobHistory{},
		startMerger: make(chan chan error),
		accuracyCh:  make(chan struct{}, 1),
		wgIteration: &sync.WaitGroup{},
		merged:      make(chan struct{}),
		stopChan:    make(chan struct{}, 1),
	}

	// extract the settings from the task
	job.extractTaskSettings(*task)

	var psUrl string
	if util.IsDebugEnv() {
		psUrl = fmt.Sprintf("http://localhost:%v", api.ParameterServerPortDebug)
	} else {
		psUrl = api.ParameterServerUrl
	}
	job.ps = psClient.MakeClient(job.logger, psUrl)
	job.optimizer = model.MakeParallelSGD(job.logger)

	return job

}

// NewBasicJob creates a job with no task provided yet. It will start the job api and
// wait for its task to be defined there.
//
// This is the constructor used when deploying the jobs in separate pods
func NewBasicJob(logger *zap.Logger, jobId string) *TrainJob {
	logger.Info("Creating new basic train job")

	job := &TrainJob{
		logger:      logger.Named(fmt.Sprintf("trainJob-%s", jobId)),
		jobId:       jobId,
		schedulerCh: make(chan *api.JobState),
		redisPool:   util.GetRedisConnectionPool(),
		history:     api.JobHistory{},
		startMerger: make(chan chan error),
		accuracyCh:  make(chan struct{}, 1),
		wgIteration: &sync.WaitGroup{},
		merged:      make(chan struct{}),
		stopChan:    make(chan struct{}, 1),
	}

	job.scheduler = schedulerClient.MakeClient(job.logger, api.SchedulerUrl)
	job.ps = psClient.MakeClient(job.logger, api.ParameterServerUrl)
	job.optimizer = model.MakeParallelSGD(job.logger)

	return job
}

// extractTaskSettings takes the train task and sets the variables used by the job
func (job *TrainJob) extractTaskSettings(task api.TrainTask) {
	job.task = &task
	job.parallelism = task.Job.State.Parallelism
	job.static = task.Parameters.Options.StaticParallelism
	job.validateEvery = task.Parameters.Options.ValidateEvery
	job.K = task.Parameters.Options.K
	job.goalAccuracy = task.Parameters.Options.GoalAccuracy
}

// Train is the main
//
// Waits for the API to receive all the requests for starting the next epoch
// After this the job needs to send a request to the scheduler to get the proper
// amount of functions to use in the next epoch
func (job *TrainJob) Train() {

	job.logger.Info("Starting to serve train job")
	job.logger.Info("Initializing model")

	defer func() {
		// After the job is finished
		// unregister the prometheus exposed metrics,
		// clear connections and send the finish signal to the parameter
		// server
		job.clearTensors()
		job.redisPool.Close()
		job.logger.Debug("closing job", zap.Error(job.exitErr))
		job.ps.JobFinished(job.jobId, job.exitErr)
	}()

	// Call the init function and build the reference model,
	// fatal if it fails
	err := job.init()
	if err != nil {
		job.logger.Error("Could not initialize model",
			zap.Error(err))
		job.exitErr = err
		return
	}

	// Main training loop
	job.startTime = time.Now()

main:
	for job.epoch = 1; job.epoch <= job.task.Parameters.Epochs; job.epoch++ {

		err := job.train()
		if err != nil {
			job.logger.Error("Error training model", zap.Error(err))
			job.exitErr = err
			return
		}

		// If we need, ask the scheduler for updated settings
		if !job.static && job.epoch < job.task.Parameters.Epochs {
			err = job.scheduler.UpdateJob(job.task)
			if err != nil {
				job.logger.Error("Error updating parallelism",
					zap.Error(err))
				continue
			}

			update := <-job.schedulerCh
			job.logger.Info("Received next config from the Scheduler",
				zap.Int("new parallelism", update.Parallelism))

			// Get the new parallelism and update it in the history
			job.task.Job.State = *update
			if !util.IsDebugEnv() && !util.LimitParallelism() {
				job.logger.Debug("updating parallelism...")
				job.parallelism = update.Parallelism
			}

		}

		// receive signal that the models are merged
		job.logger.Debug("Waiting for merge to complete...")
		<-job.merged

		// Trigger validation if configured
		if job.validateEvery != 0 &&
			job.epoch%job.validateEvery == 0 &&
			job.epoch != job.task.Parameters.Epochs {

			err = job.validate()
			if err != nil {
				job.logger.Error("error performing validation",
					zap.Error(err))
			}
		}

		// check if the validation returned and we reached the goal average
		select {
		case <-job.stopChan:
			job.logger.Debug("Job stopping...")
			job.accuracyReached = true
			job.exitErr = errors.New("job was force stopped")
			break main
		case <-job.accuracyCh:
			job.logger.Debug("goal accuracy reached!, exiting")
			job.accuracyReached = true
			break main
		default:
		}
	}

	// if the accuracy is already reached, no need to
	// validate again
	if !job.accuracyReached {
		err = job.validate()
		if err != nil {
			job.logger.Error("error performing validation",
				zap.Error(err))
		}
	}

	// Wait for the val functions to finish if there
	// are still some running
	job.saveTrainingHistory()

	job.logger.Info("Exiting...", zap.Any("history", job.history))
	job.logger.Info(fmt.Sprintf("Training finished after %d epochs", job.epoch-1))

}

// init launches the function and creates the model used by the TrainJob
func (job *TrainJob) init() error {

	job.logger.Debug("Calling init function")
	layers, err := job.invokeInitFunction()
	if err != nil {
		return errors.Wrap(err, "error invoking init function")
	}
	if len(layers) == 0 {
		return errors.New("length of the layers is zero")
	}

	job.logger.Debug("Received layers", zap.Any("layers", layers))
	job.logger.Debug("Creating model")
	m := model.NewModel(job.logger, job.jobId, job.task.Parameters, layers, job.redisPool)
	job.model = m

	err = m.Build()
	if err != nil {
		return errors.Wrap(err, "error building model")
	}

	m.Summary()
	return nil
}

// train invokes the functions in each train stage and
// returns the total time that the model spent training
func (job *TrainJob) train() error {
	job.logger.Info("Started new epoch", zap.Int("epoch", job.epoch))

	// set the channels and wait groups for the
	// K-AVG model merger to receive models from the
	// functions every K local forward passes
	job.finishCh = make(chan *finishNotification, job.parallelism)
	job.wgIteration.Add(job.parallelism)
	atomic.StoreInt64(&job.finishedFuncs, 0)
	errChan := make(chan error, 1)
	job.startMerger <- errChan

	start := time.Now()
	loss, _, err := job.invokeTrainFunctions()
	if err != nil {
		return errors.Wrap(err, "error invoking functions")
	}

	// check if there was an error merging the model
	select {
	case err := <-errChan:
		return errors.Wrap(err, "error merging model")
	default:
	}

	// update the elapsed time
	elapsed := time.Since(start)
	job.task.Job.State.ElapsedTime = elapsed.Seconds()

	job.logger.Info("Epoch finished")

	// update the training metrics
	err = job.updateTrainMetrics(loss, time.Since(job.startTime))
	if err != nil {
		job.logger.Error("error updating metrics", zap.Error(err))
	}

	job.logger.Debug("History updated", zap.Any("history", job.history))
	return nil
}

// validate invokes the validation functions
// it uses the same degree of parallelism as the train functions and
// averages the results from the functions later
func (job *TrainJob) validate() error {
	// invoke the validation function concurrently
	accuracy, loss, err := job.invokeValFunctions()
	if err != nil {
		return errors.Wrap(err, "error during validation")
	}

	err = job.updateValidationMetrics(loss, accuracy)
	if err != nil {
		return errors.Wrap(err, "error sending val results")
	}

	job.logger.Debug("History updated", zap.Any("history", job.history))

	// if the accuracy reached the goal, send the notification
	if accuracy >= job.goalAccuracy {
		job.logger.Debug("goal accuracy reached, sending message",
			zap.Float64("goal", job.goalAccuracy),
			zap.Float64("acc", accuracy))
		job.accuracyCh <- struct{}{}
	}

	return nil
}

// mergeModel waits for a signal to start listening to functions requests
//
// After all running functions completing, it iterates through the function notifications
// and merges the layers from those functions before allowing functions to continue to the next iteration
func (job *TrainJob) mergeModel() {

	for {
		errChan := <-job.startMerger

		for {
			job.model.Clear()
			job.logger.Debug("Waiting for functions to finish...")
			job.wgIteration.Wait()

			// get the function ids that will be taken into account
			// when fetching and merging the model
			var funcs []int
			var channels []chan MergeResult
			close(job.finishCh)
			for msg := range job.finishCh {
				funcs = append(funcs, msg.funcId)
				channels = append(channels, msg.respChan)
			}

			if len(funcs) == 0 {
				errChan <- errors.New("no functions returned for merging")
				break
			}

			// once all are done, merge the model and update
			job.logger.Debug("Merging models after iteration", zap.Ints("finishCh", funcs))

			// time the merge time for tests
			mergeStart := time.Now()
			err := job.optimizer.Average(job.model, len(funcs))
			if err != nil {
				answerFunctions(MergeFailed, channels)
				errChan <- err
				break
			}

			err = job.model.Save()
			if err != nil {
				job.logger.Error("error saving model", zap.Error(err))
				answerFunctions(MergeFailed, channels)
				errChan <- err
				break
			}
			job.logger.Debug("Merge and save took", zap.Float64("time", time.Since(mergeStart).Seconds()))

			finished := atomic.LoadInt64(&job.finishedFuncs)
			job.logger.Debug("finished funcs are", zap.Int64("num", finished))
			// initialize the wait group again by checking the number of finished functions
			remaining := job.parallelism - int(finished)
			if remaining == 0 {
				job.logger.Debug("all functions finished, quiting...")

				// communicate that the model is ready
				job.merged <- struct{}{}

				break

			} else {
				job.logger.Debug("remaining functions is", zap.Int("num", remaining))
				// reset the wait group and reopen the channel with a buffer
				// size equal to the number of finishCh
				job.wgIteration.Add(remaining)
				job.finishCh = make(chan *finishNotification, remaining)

				// answer to all the non-nil channels
				// a channel is nil if the functions is completely finished
				// it might be that some functions have to do 1 more iteration,
				// so those send a nil channel
				answerFunctions(MergeSucceeded, channels)
			}
		}
	}

}

// answerFunctions responds to functions with the result of the merging process
func answerFunctions(result MergeResult, channels []chan MergeResult) {
	for _, ch := range channels {
		if ch != nil {
			ch <- result
		}
	}
}
