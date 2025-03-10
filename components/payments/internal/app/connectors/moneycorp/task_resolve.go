package moneycorp

import (
	"fmt"

	"github.com/formancehq/payments/internal/app/connectors/moneycorp/client"
	"github.com/formancehq/payments/internal/app/task"
	"github.com/formancehq/stack/libs/go-libs/logging"
)

const (
	taskNameMain                = "main"
	taskNameFetchAccounts       = "fetch-accounts"
	taskNameFetchRecipients     = "fetch-recipients"
	taskNameFetchTransactions   = "fetch-transactions"
	taskNameFetchBalances       = "fetch-balances"
	taskNameInitiatePayment     = "initiate-payment"
	taskNameUpdatePaymentStatus = "update-payment-status"
)

// TaskDescriptor is the definition of a task.
type TaskDescriptor struct {
	Name       string `json:"name" yaml:"name" bson:"name"`
	Key        string `json:"key" yaml:"key" bson:"key"`
	AccountID  string `json:"accountID" yaml:"accountID" bson:"accountID"`
	TransferID string `json:"transferID" yaml:"transferID" bson:"transferID"`
	PaymentID  string `json:"paymentID" yaml:"paymentID" bson:"paymentID"`
	Attempt    int    `json:"attempt" yaml:"attempt" bson:"attempt"`
}

// clientID, apiKey, endpoint string, logger logging
func resolveTasks(logger logging.Logger, config Config) func(taskDefinition TaskDescriptor) task.Task {
	moneycorpClient, err := client.NewClient(
		config.ClientID,
		config.APIKey,
		config.Endpoint,
		logger,
	)
	if err != nil {
		logger.Error(err)

		return nil
	}

	return func(taskDescriptor TaskDescriptor) task.Task {
		switch taskDescriptor.Key {
		case taskNameMain:
			return taskMain(logger)
		case taskNameFetchAccounts:
			return taskFetchAccounts(logger, moneycorpClient)
		case taskNameFetchRecipients:
			return taskFetchRecipients(logger, moneycorpClient, taskDescriptor.AccountID)
		case taskNameFetchTransactions:
			return taskFetchTransactions(logger, moneycorpClient, taskDescriptor.AccountID)
		case taskNameFetchBalances:
			return taskFetchBalances(logger, moneycorpClient, taskDescriptor.AccountID)
		case taskNameInitiatePayment:
			return taskInitiatePayment(logger, moneycorpClient, taskDescriptor.TransferID)
		case taskNameUpdatePaymentStatus:
			return taskUpdatePaymentStatus(logger, moneycorpClient, taskDescriptor.TransferID, taskDescriptor.PaymentID, taskDescriptor.Attempt)
		}

		// This should never happen.
		return func() error {
			return fmt.Errorf("key '%s': %w", taskDescriptor.Key, ErrMissingTask)
		}
	}
}
