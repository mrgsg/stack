package moneycorp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/formancehq/payments/internal/app/connectors/currency"
	"github.com/formancehq/payments/internal/app/connectors/moneycorp/client"
	"github.com/formancehq/payments/internal/app/ingestion"
	"github.com/formancehq/payments/internal/app/metrics"
	"github.com/formancehq/payments/internal/app/models"
	"github.com/formancehq/payments/internal/app/storage"
	"github.com/formancehq/payments/internal/app/task"
	"github.com/formancehq/stack/libs/go-libs/contextutil"
	"github.com/formancehq/stack/libs/go-libs/logging"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	initiateTransferAttrs = metric.WithAttributes(append(connectorAttrs, attribute.String(metrics.ObjectAttributeKey, "initiate_transfer"))...)
	initiatePayoutAttrs   = metric.WithAttributes(append(connectorAttrs, attribute.String(metrics.ObjectAttributeKey, "initiate_payout"))...)
)

func taskInitiatePayment(logger logging.Logger, moneycorpClient *client.Client, transferID string) task.Task {
	return func(
		ctx context.Context,
		ingester ingestion.Ingester,
		scheduler task.Scheduler,
		storageReader storage.Reader,
		metricsRegistry metrics.MetricsRegistry,
	) error {
		logger.Info("initiate payment for transfer-initiation %s", transferID)

		transferInitiationID := models.MustTransferInitiationIDFromString(transferID)

		attrs := metric.WithAttributes(connectorAttrs...)
		var err error
		var paymentID *models.PaymentID
		defer func() {
			if err != nil {
				ctx, cancel := contextutil.Detached(ctx)
				defer cancel()
				metricsRegistry.ConnectorObjectsErrors().Add(ctx, 1, attrs)
				if err := ingester.UpdateTransferInitiationPaymentsStatus(ctx, transferInitiationID, paymentID, models.TransferInitiationStatusFailed, err.Error(), 0, time.Now()); err != nil {
					logger.Error("failed to update transfer initiation status: %v", err)
				}
			}
		}()

		err = ingester.UpdateTransferInitiationPaymentsStatus(ctx, transferInitiationID, paymentID, models.TransferInitiationStatusProcessing, "", 0, time.Now())
		if err != nil {
			return err
		}

		var transfer *models.TransferInitiation
		transfer, err = getTransfer(ctx, storageReader, transferInitiationID, true)
		if err != nil {
			return err
		}

		attrs = initiateTransferAttrs
		if transfer.Type == models.TransferInitiationTypePayout {
			attrs = initiatePayoutAttrs
		}

		logger.Info("initiate payment between", transfer.SourceAccountID, " and %s", transfer.DestinationAccountID)

		now := time.Now()
		defer func() {
			metricsRegistry.ConnectorObjectsLatency().Record(ctx, time.Since(now).Milliseconds(), attrs)
		}()

		if transfer.SourceAccount == nil {
			err = errors.New("no source account provided")
		}

		if transfer.SourceAccount.Type == models.AccountTypeExternal {
			err = errors.New("payin not implemented: source account must be an internal account")
			return err
		}

		var curr string
		var precision int
		curr, precision, err = currency.GetCurrencyAndPrecisionFromAsset(transfer.Asset)
		if err != nil {
			return err
		}

		transferAmount := big.NewFloat(0).SetInt(transfer.Amount)
		amount, accuracy := transferAmount.Quo(transferAmount, big.NewFloat(math.Pow(10, float64(precision)))).Float64()
		if accuracy != big.Exact {
			return errors.New("amount is not accurate, psp does not support big ints")
		}

		var connectorPaymentID string
		var paymentType models.PaymentType
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		switch transfer.DestinationAccount.Type {
		case models.AccountTypeInternal:
			// Transfer between internal accounts
			var resp *client.TransferResponse
			resp, err = moneycorpClient.InitiateTransfer(ctx, &client.TransferRequest{
				SourceAccountID:    transfer.SourceAccountID.Reference,
				IdempotencyKey:     fmt.Sprintf("%s_%d", transfer.ID.Reference, transfer.Attempts),
				ReceivingAccountID: transfer.DestinationAccountID.Reference,
				TransferAmount:     amount,
				TransferCurrency:   curr,
				TransferReference:  transfer.Description,
				ClientReference:    transfer.Description,
			})
			if err != nil {
				return err
			}

			connectorPaymentID = resp.ID
			paymentType = models.PaymentTypeTransfer
		case models.AccountTypeExternal:
			// Payout to an external account
			var resp *client.PayoutResponse
			resp, err = moneycorpClient.InitiatePayout(ctx, &client.PayoutRequest{
				SourceAccountID:  transfer.SourceAccountID.Reference,
				IdempotencyKey:   fmt.Sprintf("%s_%d", transfer.ID.Reference, transfer.Attempts),
				RecipientID:      transfer.DestinationAccountID.Reference,
				PaymentAmount:    amount,
				PaymentCurrency:  curr,
				PaymentMethgod:   "Standard",
				PaymentReference: transfer.Description,
				ClientReference:  transfer.Description,
			})
			if err != nil {
				return err
			}

			connectorPaymentID = resp.ID
			paymentType = models.PaymentTypePayOut
		}
		metricsRegistry.ConnectorObjects().Add(ctx, 1, attrs)

		paymentID = &models.PaymentID{
			PaymentReference: models.PaymentReference{
				Reference: connectorPaymentID,
				Type:      paymentType,
			},
			Provider: models.ConnectorProviderMoneycorp,
		}
		err = ingester.AddTransferInitiationPaymentID(ctx, transferInitiationID, paymentID, time.Now())
		if err != nil {
			return err
		}

		taskDescriptor, err := models.EncodeTaskDescriptor(TaskDescriptor{
			Name:       "Update transfer initiation status",
			Key:        taskNameUpdatePaymentStatus,
			TransferID: transfer.ID.String(),
			PaymentID:  paymentID.String(),
			Attempt:    1,
		})
		if err != nil {
			return err
		}

		ctx, _ = contextutil.DetachedWithTimeout(ctx, 10*time.Second)
		err = scheduler.Schedule(ctx, taskDescriptor, models.TaskSchedulerOptions{
			ScheduleOption: models.OPTIONS_RUN_NOW,
			RestartOption:  models.OPTIONS_RESTART_IF_NOT_ACTIVE,
		})
		if err != nil && !errors.Is(err, task.ErrAlreadyScheduled) {
			return err
		}

		return nil
	}
}

var (
	updateTransferAttrs = metric.WithAttributes(append(connectorAttrs, attribute.String(metrics.ObjectAttributeKey, "update_transfer"))...)
	updatePayoutAttrs   = metric.WithAttributes(append(connectorAttrs, attribute.String(metrics.ObjectAttributeKey, "update_payout"))...)
)

func taskUpdatePaymentStatus(
	logger logging.Logger,
	moneycorpClient *client.Client,
	transferID string,
	pID string,
	attempt int,
) task.Task {
	return func(
		ctx context.Context,
		ingester ingestion.Ingester,
		scheduler task.Scheduler,
		storageReader storage.Reader,
		metricsRegistry metrics.MetricsRegistry,
	) error {
		paymentID := models.MustPaymentIDFromString(pID)
		transferInitiationID := models.MustTransferInitiationIDFromString(transferID)
		transfer, err := getTransfer(ctx, storageReader, transferInitiationID, true)
		if err != nil {
			return err
		}
		logger.Info("attempt: ", attempt, " fetching status of ", pID)

		attrs := updateTransferAttrs
		if transfer.Type == models.TransferInitiationTypePayout {
			attrs = updatePayoutAttrs
		}

		now := time.Now()
		defer func() {
			metricsRegistry.ConnectorObjectsLatency().Record(ctx, time.Since(now).Milliseconds(), attrs)
		}()

		defer func() {
			if err != nil {
				metricsRegistry.ConnectorObjectsErrors().Add(ctx, 1, attrs)
			}
		}()

		var status string
		var resultMessage string
		switch transfer.Type {
		case models.TransferInitiationTypeTransfer:
			var resp *client.TransferResponse
			resp, err = moneycorpClient.GetTransfer(ctx, transfer.SourceAccount.Reference, paymentID.Reference)
			if err != nil {
				return err
			}

			status = resp.Attributes.TransferStatus
		case models.TransferInitiationTypePayout:
			var resp *client.PayoutResponse
			resp, err = moneycorpClient.GetPayout(ctx, transfer.SourceAccount.Reference, paymentID.Reference)
			if err != nil {
				return err
			}

			status = resp.Attributes.PaymentStatus
		}

		switch status {
		case "Awaiting Dispatch":
			taskDescriptor, err := models.EncodeTaskDescriptor(TaskDescriptor{
				Name:       "Update transfer initiation status",
				Key:        taskNameUpdatePaymentStatus,
				TransferID: transfer.ID.String(),
				Attempt:    attempt + 1,
			})
			if err != nil {
				return err
			}

			err = scheduler.Schedule(ctx, taskDescriptor, models.TaskSchedulerOptions{
				ScheduleOption: models.OPTIONS_RUN_IN_DURATION,
				Duration:       2 * time.Minute,
				RestartOption:  models.OPTIONS_RESTART_IF_NOT_ACTIVE,
			})
			if err != nil && !errors.Is(err, task.ErrAlreadyScheduled) {
				return err
			}
		case "Cleared", "Sent":
			err = ingester.UpdateTransferInitiationPaymentsStatus(ctx, transferInitiationID, paymentID, models.TransferInitiationStatusProcessed, "", 0, time.Now())
			if err != nil {
				return err
			}

			return nil
		case "Unauthorised", "Failed", "Cancelled":
			err = ingester.UpdateTransferInitiationPaymentsStatus(ctx, transferInitiationID, paymentID, models.TransferInitiationStatusFailed, resultMessage, 0, time.Now())
			if err != nil {
				return err
			}

			return nil
		}

		return nil
	}
}

func getTransfer(
	ctx context.Context,
	reader storage.Reader,
	transferID models.TransferInitiationID,
	expand bool,
) (*models.TransferInitiation, error) {
	transfer, err := reader.ReadTransferInitiation(ctx, transferID)
	if err != nil {
		return nil, err
	}

	if expand {
		sourceAccount, err := reader.GetAccount(ctx, transfer.SourceAccountID.String())
		if err != nil {
			return nil, err
		}
		transfer.SourceAccount = sourceAccount

		destinationAccount, err := reader.GetAccount(ctx, transfer.DestinationAccountID.String())
		if err != nil {
			return nil, err
		}
		transfer.DestinationAccount = destinationAccount
	}

	return transfer, nil
}
