package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type Payment struct {
	Id             string `json:"Id"`
	Tag            string `json:"Tag"`
	CreationDate   int64  `json:"CreationDate"`
	AuthorId       string `json:"AuthorId"`
	CreditedUserId string `json:"CreditedUserId"`
	DebitedFunds   struct {
		Currency string      `json:"Currency"`
		Amount   json.Number `json:"Amount"`
	} `json:"DebitedFunds"`
	CreditedFunds struct {
		Currency string      `json:"Currency"`
		Amount   json.Number `json:"Amount"`
	} `json:"CreditedFunds"`
	Fees struct {
		Currency string      `json:"Currency"`
		Amount   json.Number `json:"Amount"`
	} `json:"Fees"`
	Status           string `json:"Status"`
	ResultCode       string `json:"ResultCode"`
	ResultMessage    string `json:"ResultMessage"`
	ExecutionDate    int64  `json:"ExecutionDate"`
	Type             string `json:"Type"`
	Nature           string `json:"Nature"`
	CreditedWalletID string `json:"CreditedWalletId"`
	DebitedWalletID  string `json:"DebitedWalletId"`
}

func (c *Client) GetTransactions(ctx context.Context, walletsID string, page int) ([]*Payment, error) {
	endpoint := fmt.Sprintf("%s/v2.01/%s/wallets/%s/transactions", c.endpoint, c.clientID, walletsID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create login request: %w", err)
	}

	q := req.URL.Query()
	q.Add("per_page", "100")
	q.Add("page", fmt.Sprint(page))
	req.URL.RawQuery = q.Encode()

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get transactions: %w", err)
	}

	defer func() {
		err = resp.Body.Close()
		if err != nil {
			c.logger.Error(err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, unmarshalError(resp.StatusCode, resp.Body).Error()
	}

	var payments []*Payment
	if err := json.NewDecoder(resp.Body).Decode(&payments); err != nil {
		return nil, fmt.Errorf("failed to unmarshal transactions response body: %w", err)
	}

	return payments, nil
}
