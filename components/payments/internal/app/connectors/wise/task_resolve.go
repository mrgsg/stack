package wise

import (
	"fmt"
	"math/big"

	"github.com/google/uuid"

	"github.com/formancehq/payments/internal/app/connectors/wise/client"
	"github.com/formancehq/payments/internal/app/task"
	"github.com/formancehq/stack/libs/go-libs/logging"
)

const (
	taskNameMain                   = "main"
	taskNameFetchTransfers         = "fetch-transfers"
	taskNameFetchProfiles          = "fetch-profiles"
	taskNameFetchRecipientAccounts = "fetch-recipient-accounts"
	taskNameInitiatePayment        = "initiate-payment"
	taskNameUpdatePaymentStatus    = "update-payment-status"
)

// TaskDescriptor is the definition of a task.
type TaskDescriptor struct {
	Name       string `json:"name" yaml:"name" bson:"name"`
	Key        string `json:"key" yaml:"key" bson:"key"`
	ProfileID  uint64 `json:"profileID" yaml:"profileID" bson:"profileID"`
	TransferID string `json:"transferID" yaml:"transferID" bson:"transferID"`
	PaymentID  string `json:"paymentID" yaml:"paymentID" bson:"paymentID"`
	Attempt    int    `json:"attempt" yaml:"attempt" bson:"attempt"`
}

type Transfer struct {
	ID          uuid.UUID `json:"id" yaml:"id" bson:"id"`
	Source      string    `json:"source" yaml:"source" bson:"source"`
	Destination string    `json:"destination" yaml:"destination" bson:"destination"`
	Amount      *big.Int  `json:"amount" yaml:"amount" bson:"amount"`
	Currency    string    `json:"currency" yaml:"currency" bson:"currency"`
}

func resolveTasks(logger logging.Logger, config Config) func(taskDefinition TaskDescriptor) task.Task {
	client := client.NewClient(config.APIKey)

	return func(taskDefinition TaskDescriptor) task.Task {
		switch taskDefinition.Key {
		case taskNameMain:
			return taskMain(logger)
		case taskNameFetchProfiles:
			return taskFetchProfiles(logger, client)
		case taskNameFetchRecipientAccounts:
			return taskFetchRecipientAccounts(logger, client, taskDefinition.ProfileID)
		case taskNameFetchTransfers:
			return taskFetchTransfers(logger, client, taskDefinition.ProfileID)
		case taskNameInitiatePayment:
			return taskInitiatePayment(logger, client, taskDefinition.TransferID)
		case taskNameUpdatePaymentStatus:
			return taskUpdatePaymentStatus(logger, client, taskDefinition.TransferID, taskDefinition.PaymentID, taskDefinition.Attempt)
		}

		// This should never happen.
		return func() error {
			return fmt.Errorf("key '%s': %w", taskDefinition.Key, ErrMissingTask)
		}
	}
}
