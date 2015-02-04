package fsm

import (
	"errors"
	"fmt"
	"log"
	"reflect"

	"github.com/awslabs/aws-sdk-go/aws"
	"github.com/awslabs/aws-sdk-go/gen/swf"
	"github.com/sclasen/swfsm/poller"
	. "github.com/sclasen/swfsm/sugar"
)

//SWFOps is the subset of swf.SWF ops required by the fsm package
type SWFOps interface {
	PollForDecisionTask(*swf.PollForDecisionTaskInput) (*swf.DecisionTask, error)
	PollForActivityTask(*swf.PollForActivityTaskInput) (*swf.ActivityTask, error)
	RespondDecisionTaskCompleted(*swf.RespondDecisionTaskCompletedInput) error
}

// FSM models the decision handling logic a workflow in SWF
type FSM struct {
	//Name of the fsm. Used when emitting logs. Should probably be set to the name of the workflow associated with the fsm.
	Name string
	// Domain of the workflow associated with the FSM.
	Domain string
	// TaskList that the underlying poller will poll for decision tasks.
	TaskList string
	// Identity used in PollForDecisionTaskRequests, can be empty.
	Identity string
	// Client used to make SWF api requests.
	SWF SWFOps
	// Strategy for replication of state to the systems the build the Query side model.
	ReplicationHandler ReplicationHandler
	// DataType of the data struct associated with this FSM.
	// The data is automatically peristed to and loaded from workflow history by the FSM.
	DataType interface{}
	// Serializer used to serialize/deserialise fsm state data to/from workflow history.
	Serializer StateSerializer
	// Serializer used to serialize/deserialise in json the fsm managed marker recorded events to/from workflow history.
	systemSerializer StateSerializer
	//PollerShutdownManager is used when the FSM is managing the polling
	ShutdownManager *poller.ShutdownManager
	//DecisionTaskDispatcher determines the concurrency strategy for processing tasks in your fsm
	DecisionTaskDispatcher DecisionTaskDispatcher
	//DecisionErrorHandler  is called whenever there is a panic in your decider. if it returns a non-nil error, the attempt to handle the DecisionTask is abandoned and will time out.
	DecisionErrorHandler DecisionErrorHandler
	//FSMErrorHandler  is called whenever there is an error within the FSM, usually indicating bad state or configuration of your FSM. if it returns a non-nil error, the attempt to handle the DecisionTask is abandoned and will time out.
	FSMErrorReporter FSMErrorReporter
	states           map[string]*FSMState
	initialState     *FSMState
	completeState    *FSMState
	stop             chan bool
	stopAck          chan bool
	allowPanics      bool //makes testing easier
}

// StateSerializer is the implementation of FSMSerializer.StateSerializer()
func (f *FSM) StateSerializer() StateSerializer {
	return f.Serializer
}

// AddInitialState adds a state to the FSM and uses it as the initial state when a workflow execution is started.
func (f *FSM) AddInitialState(state *FSMState) {
	f.AddState(state)
	f.initialState = state
}

// InitialState is the implementation of FSMSerializer.InitialState()
func (f *FSM) InitialState() string {
	return f.initialState.Name
}

// AddState adds a state to the FSM.
func (f *FSM) AddState(state *FSMState) {
	if f.states == nil {
		f.states = make(map[string]*FSMState)
	}
	f.states[state.Name] = state
}

// AddCompleteState adds a state to the FSM and uses it as the final state of a workflow.
// it will only receive events if you returned FSMContext.Complete(...) and the workflow was unable to complete.
func (f *FSM) AddCompleteState(state *FSMState) {
	f.AddState(state)
	f.completeState = state
}

// DefaultCompleteState is the complete state used in an FSM if one has not been set.
// It simply responds with a CompleteDecision which attempts to Complete the workflow.
// This state will only get events if you previously attempted to complete the workflow and it failed.
func (f *FSM) DefaultCompleteState() *FSMState {
	return &FSMState{
		Name: CompleteState,
		Decider: func(fsm *FSMContext, h swf.HistoryEvent, data interface{}) Outcome {
			f.log("state=complete at=attempt-completion event=%s", h)
			return fsm.CompleteWorkflow(data)
		},
	}
}

func (f *FSM) DefaultDecisionErrorHandler(ctx *FSMContext, event swf.HistoryEvent, stateBeforeEvent interface{}, stateAfterError interface{}, err error) (*Outcome, error) {
	return nil, err
}

func (f *FSM) DefaultFSMErrorHandler(ctx *FSMContext, event swf.HistoryEvent, stateData interface{}, errorType string, err error) (*Outcome, error) {
	return nil, err
}

// Init initializaed any optional, unspecified values such as the error state, stop channel, serializer, PollerShutdownManager.
// it gets called by Start(), so you should only call this if you are manually managing polling for tasks, and calling Tick yourself.
func (f *FSM) Init() {
	if f.initialState == nil {
		panic("No Initial State Defined For FSM")
	}

	if f.completeState == nil {
		f.AddCompleteState(f.DefaultCompleteState())
	}

	if f.stop == nil {
		f.stop = make(chan bool, 1)
	}

	if f.stopAck == nil {
		f.stopAck = make(chan bool, 1)
	}

	if f.Serializer == nil {
		f.log("action=start at=no-serializer defaulting-to=JSONSerializer")
		f.Serializer = &JSONStateSerializer{}
	}

	if f.systemSerializer == nil {
		f.log("action=start at=no-system-serializer defaulting-to=JSONSerializer")
		f.systemSerializer = &JSONStateSerializer{}
	}

	if f.ShutdownManager == nil {
		f.ShutdownManager = poller.NewShutdownManager()
	}

	if f.DecisionTaskDispatcher == nil {
		f.DecisionTaskDispatcher = &CallingGoroutineDispatcher{}
	}

	if f.DecisionErrorHandler == nil {
		f.DecisionErrorHandler = f.DefaultDecisionErrorHandler
	}

}

// Start begins processing DecisionTasks with the FSM. It creates a DecisionTaskPoller and spawns a goroutine that continues polling until Stop() is called and any in-flight polls have completed.
// If you wish to manage polling and calling Tick() yourself, you dont need to start the FSM, just call Init().
func (f *FSM) Start() {
	f.Init()
	poller := poller.NewDecisionTaskPoller(f.SWF, f.Domain, f.Identity, f.TaskList)
	go poller.PollUntilShutdownBy(f.ShutdownManager, fmt.Sprintf("%s-poller", f.Name), f.dispatchTask)
}

func (f *FSM) dispatchTask(decisionTask *swf.DecisionTask) {
	f.DecisionTaskDispatcher.DispatchTask(decisionTask, f.handleDecisionTask)
}

func (f *FSM) handleDecisionTask(decisionTask *swf.DecisionTask) {
	context, decisions, state, err := f.Tick(decisionTask)
	if err != nil {
		f.log("action=tick at=tick-error status=abandoning-task error=%q", err.Error())
		return
	}
	complete := &swf.RespondDecisionTaskCompletedInput{
		Decisions: decisions,
		TaskToken: decisionTask.TaskToken,
	}

	complete.ExecutionContext = aws.String(state.StateName)

	if err := f.SWF.RespondDecisionTaskCompleted(complete); err != nil {
		f.log("action=tick at=decide-request-failed error=%q", err.Error())
		return
	}

	if f.ReplicationHandler != nil {
		repErr := f.ReplicationHandler(context, decisionTask, complete, state)
		if repErr != nil {
			f.log("action=tick at=replication-handler-failed error=%q", repErr.Error())
		}
	}

}

// Serialize uses the FSM.Serializer to serialize data to a string.
// If there is an error in serialization this func will panic, so this should usually only be used inside Deciders
// where the panics are recovered and proper errors are recorded in the workflow.
func (f *FSM) Serialize(data interface{}) string {
	serialized, err := f.Serializer.Serialize(data)
	if err != nil {
		panic(err)
	}
	return serialized
}

// Deserialize uses the FSM.Serializer to deserialize data from a string.
// If there is an error in deserialization this func will panic, so this should usually only be used inside Deciders
// where the panics are recovered and proper errors are recorded in the workflow.
func (f *FSM) Deserialize(serialized string, data interface{}) {
	err := f.Serializer.Deserialize(serialized, data)
	if err != nil {
		panic(err)
	}
	return
}

// Tick is called when the DecisionTaskPoller receives a PollForDecisionTaskResponse in its polling loop.
// On errors, a nil *SerializedState is returned, and an error Outcome is included in the Decision list.
// It is exported to facilitate testing.
func (f *FSM) Tick(decisionTask *swf.DecisionTask) (*FSMContext, []swf.Decision, *SerializedState, error) {
	lastEvents := f.findLastEvents(*decisionTask.PreviousStartedEventID, decisionTask.Events)
	outcome := new(Outcome)
	context := NewFSMContext(f,
		*decisionTask.WorkflowType,
		*decisionTask.WorkflowExecution,
		nil,
		"", nil, uint64(0),
	)

	serializedState, err := f.findSerializedState(decisionTask.Events)
	if err != nil {
		f.log("action=tick at=error=find-serialized-state-failed err=%q", err)
		f.FSMErrorReporter.ErrorFindingStateData(decisionTask, err)
		if f.allowPanics {
			panic(err)
		}
		return nil, nil, nil, err
	}
	eventCorrelator, err := f.findSerializedEventCorrelator(decisionTask.Events)
	if err != nil {
		f.log("action=tick at=error=find-serialized-event-correlator-failed err=%q", err)
		f.FSMErrorReporter.ErrorFindingCorrelator(decisionTask, err)
		if f.allowPanics {
			panic(err)
		}
		return nil, nil, nil, err
	}
	context.eventCorrelator = eventCorrelator

	f.log("action=tick at=find-serialized-state state=%s", serializedState.StateName)

	if outcome.Data == nil && outcome.State == "" {
		data := reflect.New(reflect.TypeOf(f.DataType)).Interface()
		if err = f.Serializer.Deserialize(serializedState.StateData, data); err != nil {
			f.log("action=tick at=error=deserialize-state-failed err=&s", err)
			f.FSMErrorReporter.ErrorDeserializingStateData(decisionTask, serializedState.StateData, err)
			if f.allowPanics {
				panic(err)
			}
			return nil, nil, nil, err
		}
		f.log("action=tick at=find-current-data data=%v", data)
		outcome.Data = data
		outcome.State = serializedState.StateName
		context.stateVersion = serializedState.StateVersion
	}

	//iterate through events oldest to newest, calling the decider for the current state.
	//if the outcome changes the state use the right FSMState
	for i := len(lastEvents) - 1; i >= 0; i-- {
		e := lastEvents[i]
		f.log("action=tick at=history id=%d type=%s", e.EventID, e.EventType)
		fsmState, ok := f.states[outcome.State]
		if ok {
			context.State = outcome.State
			context.stateData = outcome.Data
			//stash a copy of the state before the decision in case we need to call the error handler
			stashed := f.Serialize(outcome.Data)
			anOutcome, err := f.panicSafeDecide(fsmState, context, e, outcome.Data)
			if err != nil {
				stashedData := reflect.New(reflect.TypeOf(f.DataType)).Interface()
				f.Deserialize(stashed, stashedData)
				rescued, notRescued := f.DecisionErrorHandler(context, e, stashedData, outcome.Data, err)
				if rescued != nil {
					anOutcome = *rescued
				} else {
					return nil, nil, nil, notRescued
				}
			}
			eventCorrelator.Track(e)
			curr := outcome.State
			f.mergeOutcomes(outcome, anOutcome)
			f.log("action=tick at=decided-event state=%s next-state=%s decisions=%d", curr, outcome.State, len(anOutcome.Decisions))
		} else {
			f.log("action=tick at=error error=marked-state-not-in-fsm state=%s", outcome.State)
			f.FSMErrorReporter.ErrorMissingFSMState(decisionTask, *outcome)
			return nil, nil, nil, errors.New("marked-state-not-in-fsm state=" + outcome.State)
		}
	}

	f.log("action=tick at=events-processed next-state=%s decisions=%d", outcome.State, len(outcome.Decisions))

	for _, d := range outcome.Decisions {
		f.log("action=tick at=decide next-state=%s decision=%s", outcome.State, d.DecisionType)
	}

	final, serializedState, err := f.recordStateMarkers(context.stateVersion, outcome, eventCorrelator)
	if err != nil {
		f.log("action=tick at=error error=state-serialization-error err=%q error-type=system", err)
		f.FSMErrorReporter.ErrorSerializingStateData(decisionTask, *outcome, *eventCorrelator, err)
		if f.allowPanics {
			panic(err)
		}
		return nil, nil, nil, err
	}

	return context, final, serializedState, nil
}

func (f *FSM) mergeOutcomes(final *Outcome, intermediate Outcome) {
	final.Decisions = append(final.Decisions, intermediate.Decisions...)
	final.Data = intermediate.Data
	if intermediate.State != "" {
		final.State = intermediate.State
	}
}

func (f *FSM) panicSafeDecide(state *FSMState, context *FSMContext, event swf.HistoryEvent, data interface{}) (anOutcome Outcome, anErr error) {
	defer func() {
		if !f.allowPanics {
			if r := recover(); r != nil {
				f.log("at=error error=decide-panic-recovery %v", r)
				if err, ok := r.(error); ok && err != nil {
					anErr = err
				} else {
					anErr = errors.New("panic in decider, null error, capture error state")
				}
			}
		} else {
			log.Printf("at=panic-safe-decide-allowing-panic fsm-allow-panics=%t", f.allowPanics)
		}
	}()
	anOutcome = context.Decide(event, data, state.Decider)
	return
}

// EventData works in combination with the FSM.Serializer to provide
// deserialization of data sent in a HistoryEvent. It is sugar around extracting the event payload from the proper
// field of the proper Attributes struct on the HistoryEvent
func (f *FSM) EventData(event swf.HistoryEvent, eventData interface{}) {

	if eventData != nil {
		var serialized string
		switch *event.EventType {
		case swf.EventTypeActivityTaskCompleted:
			serialized = *event.ActivityTaskCompletedEventAttributes.Result
		case swf.EventTypeChildWorkflowExecutionFailed:
			serialized = *event.ActivityTaskFailedEventAttributes.Details
		case swf.EventTypeWorkflowExecutionCompleted:
			serialized = *event.WorkflowExecutionCompletedEventAttributes.Result
		case swf.EventTypeChildWorkflowExecutionCompleted:
			serialized = *event.ChildWorkflowExecutionCompletedEventAttributes.Result
		case swf.EventTypeWorkflowExecutionSignaled:
			serialized = *event.WorkflowExecutionSignaledEventAttributes.Input
		case swf.EventTypeWorkflowExecutionStarted:
			serialized = *event.WorkflowExecutionStartedEventAttributes.Input
		case swf.EventTypeWorkflowExecutionContinuedAsNew:
			serialized = *event.WorkflowExecutionContinuedAsNewEventAttributes.Input
		}
		if serialized != "" {
			f.Deserialize(serialized, eventData)
		} else {
			panic(fmt.Sprintf("event payload was empty for %s", PrettyHistoryEvent(event)))
		}
	}

}

func (f *FSM) log(format string, data ...interface{}) {
	actualFormat := fmt.Sprintf("component=FSM name=%s %s", f.Name, format)
	log.Printf(actualFormat, data...)
}

func (f *FSM) findSerializedState(events []swf.HistoryEvent) (*SerializedState, error) {
	for _, event := range events {
		if f.isStateMarker(event) {
			state := &SerializedState{}
			err := f.Serializer.Deserialize(*event.MarkerRecordedEventAttributes.Details, state)
			return state, err
		} else if *event.EventType == swf.EventTypeWorkflowExecutionStarted {
			state := &SerializedState{}
			//If the workflow is continued, we expect a full SerializedState as Input
			if event.WorkflowExecutionStartedEventAttributes.ContinuedExecutionRunID != nil {
				err := f.Serializer.Deserialize(*event.WorkflowExecutionStartedEventAttributes.Input, state)
				if err == nil {
					if state.StateName == "" {
						state.StateName = f.initialState.Name
					}
				}
				return state, err
			} else {
				//Otherwise we expect just a stateData struct
				state.StateVersion = 0
				state.StateName = f.initialState.Name
				state.StateData = *event.WorkflowExecutionStartedEventAttributes.Input
				return state, nil
			}
		}
	}
	return nil, errors.New("Cant Find Current Data")
}

func (f *FSM) findSerializedEventCorrelator(events []swf.HistoryEvent) (*EventCorrelator, error) {
	for _, event := range events {
		if f.isCorrelatorMarker(event) {
			correlator := &EventCorrelator{}
			err := f.Serializer.Deserialize(*event.MarkerRecordedEventAttributes.Details, correlator)
			return correlator, err
		}
	}
	return &EventCorrelator{}, nil
}

func (f *FSM) findLastEvents(prevStarted int64, events []swf.HistoryEvent) []swf.HistoryEvent {
	var lastEvents []swf.HistoryEvent

	for _, event := range events {
		if *event.EventID == prevStarted {
			return lastEvents
		}
		switch *event.EventType {
		case swf.EventTypeDecisionTaskCompleted, swf.EventTypeDecisionTaskScheduled,
			swf.EventTypeDecisionTaskStarted:
			//no-op, dont even process these?
		case swf.EventTypeMarkerRecorded:
			if !f.isStateMarker(event) && !f.isCorrelatorMarker(event) {
				lastEvents = append(lastEvents, event)
			}
		default:
			lastEvents = append(lastEvents, event)
		}

	}

	return lastEvents
}

func (f *FSM) recordStateMarkers(stateVersion uint64, outcome *Outcome, eventCorrelator *EventCorrelator) ([]swf.Decision, *SerializedState, error) {
	serializedData, err := f.Serializer.Serialize(outcome.Data)

	state := &SerializedState{
		StateVersion: stateVersion + 1, //increment the version here only.
		StateName:    outcome.State,
		StateData:    serializedData,
	}
	serializedMarker, err := f.systemSerializer.Serialize(state)

	if err != nil {
		return nil, state, err
	}

	serializedCorrelator, err := f.systemSerializer.Serialize(eventCorrelator)

	if err != nil {
		return nil, state, err
	}

	d := f.recordStringMarker(StateMarker, serializedMarker)
	c := f.recordStringMarker(CorrelatorMarker, serializedCorrelator)
	decisions := f.EmptyDecisions()
	decisions = append(decisions, d, c)
	decisions = append(decisions, outcome.Decisions...)
	return decisions, state, nil
}

func (f *FSM) recordMarker(markerName string, details interface{}) (swf.Decision, error) {
	serialized, err := f.Serializer.Serialize(details)
	if err != nil {
		return swf.Decision{}, err
	}

	return f.recordStringMarker(markerName, serialized), nil
}

func (f *FSM) recordStringMarker(markerName string, details string) swf.Decision {
	return swf.Decision{
		DecisionType: aws.String(swf.DecisionTypeRecordMarker),
		RecordMarkerDecisionAttributes: &swf.RecordMarkerDecisionAttributes{
			MarkerName: aws.String(markerName),
			Details:    aws.String(details),
		},
	}
}

// Stop causes the DecisionTask select loop to exit, and to stop the DecisionTaskPoller
func (f *FSM) Stop() {
	f.stop <- true
}

func (f *FSM) isStateMarker(e swf.HistoryEvent) bool {
	return *e.EventType == swf.EventTypeMarkerRecorded && *e.MarkerRecordedEventAttributes.MarkerName == StateMarker
}

func (f *FSM) isCorrelatorMarker(e swf.HistoryEvent) bool {
	return *e.EventType == swf.EventTypeMarkerRecorded && *e.MarkerRecordedEventAttributes.MarkerName == CorrelatorMarker
}

func (f *FSM) isErrorSignal(e swf.HistoryEvent) bool {
	if *e.EventType == swf.EventTypeWorkflowExecutionSignaled {
		switch *e.WorkflowExecutionSignaledEventAttributes.SignalName {
		case SystemErrorSignal, ErrorSignal:
			return true
		default:
			return false
		}
	} else {
		return false
	}
}

// EmptyDecisions is a helper method to give you an empty decisions array for use in your Deciders.
func (f *FSM) EmptyDecisions() []swf.Decision {
	return make([]swf.Decision, 0)
}
