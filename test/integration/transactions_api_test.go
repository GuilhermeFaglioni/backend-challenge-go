//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/postgres"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
)

type transactionJSON struct {
	TransactionID         string `json:"transactionId"`
	Status                string `json:"status"`
	Kind                  string `json:"kind"`
	ProviderID            string `json:"providerId"`
	ExternalTransactionID string `json:"externalTransactionId"`
	WalletID              string `json:"walletId"`
	FailureCode           string `json:"failureCode"`
	ProcessedAt           string `json:"processedAt"`
	// The Reference the operation named, when it named one.
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId"`
	Balance                        *struct {
		Amount string `json:"amount"`
	} `json:"balance"`
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

func (a walletAPI) read(t *testing.T, client *contractClient, token, path string) (int, http.Header, string, transactionJSON) {
	t.Helper()
	status, header, raw := client.call(t.Context(), http.MethodGet, a.base+path, token, "")
	var parsed transactionJSON
	_ = json.Unmarshal([]byte(raw), &parsed)
	return status, header, raw, parsed
}

func TestTransactionReadsFollowTheContractAndShowEachState(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")

	_, _, processed := a.submit(t, a.strict, a.provider, "k-ok", operation(w, "tx-ok", "BET", "30.00"))
	_, _, rejected := a.submit(t, a.strict, a.provider, "k-no", operation(w, "tx-no", "BET", "500.00"))
	ghost := apiWallet{id: uuid.NewString(), player: w.player}
	_, _, notFound := a.submit(t, a.strict, a.provider, "k-ghost", operation(ghost, "tx-ghost", "BET", "1.00"))

	t.Run("processed", func(t *testing.T) {
		status, header, _, tx := a.read(t, a.strict, a.provider, "/wagering/transactions/"+processed.TransactionID)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "PROCESSED", tx.Status)
		require.Equal(t, "BET", tx.Kind)
		require.Equal(t, "provider-a", tx.ProviderID)
		require.NotNil(t, tx.Balance)
		require.Equal(t, "70.00", tx.Balance.Amount, "the balance of the processing")
		require.NotEmpty(t, tx.ProcessedAt)
		require.NotEmpty(t, header.Get("X-Correlation-Id"))
	})
	t.Run("rejected, with its code", func(t *testing.T) {
		status, _, _, tx := a.read(t, a.strict, a.provider, "/wagering/transactions/"+rejected.TransactionID)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "REJECTED", tx.Status)
		require.Equal(t, "INSUFFICIENT_FUNDS", tx.FailureCode)
		require.Nil(t, tx.Balance)
	})
	t.Run("a wallet that does not exist keeps the id that was asked for", func(t *testing.T) {
		status, _, _, tx := a.read(t, a.strict, a.provider, "/wagering/transactions/"+notFound.TransactionID)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "WALLET_NOT_FOUND", tx.FailureCode)
		require.Equal(t, ghost.id, tx.WalletID)
	})
	t.Run("by external id", func(t *testing.T) {
		status, _, _, tx := a.read(t, a.strict, a.provider, "/providers/provider-a/wagering/transactions/tx-ok")
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, processed.TransactionID, tx.TransactionID)
		require.Equal(t, "tx-ok", tx.ExternalTransactionID)
	})
	t.Run("the internal service reads any, including the opening of a wallet", func(t *testing.T) {
		status, _, _, tx := a.read(t, a.strict, a.internal, "/wagering/transactions/"+processed.TransactionID)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "PROCESSED", tx.Status)

		var openingID string
		require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT id FROM wager_transactions WHERE kind = 'OPENING' AND wallet_id = $1`, w.id).Scan(&openingID))
		status, _, raw, opening := a.read(t, a.strict, a.internal, "/wagering/transactions/"+openingID)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "OPENING", opening.Kind)
		require.Empty(t, opening.ProviderID, raw)
		status, _, _, _ = a.read(t, a.strict, a.internal, "/providers/provider-a/wagering/transactions/tx-ok")
		require.Equal(t, http.StatusOK, status)
	})
	a.strict.requireNoViolations()
}

func TestAProviderOnlyEverReachesItsOwnTransactions(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	_, _, mine := a.submit(t, a.strict, a.provider, "k-1", operation(w, "tx-1", "BET", "10.00"))
	var openingID string
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT id FROM wager_transactions WHERE kind = 'OPENING' AND wallet_id = $1`, w.id).Scan(&openingID))
	before := a.tables(t)

	// Provider B asks for provider A's transaction, by every door it has.
	byID, _, bodyByID, _ := a.read(t, a.client, a.providerB, "/wagering/transactions/"+mine.TransactionID)
	unknown, _, bodyUnknown, _ := a.read(t, a.client, a.providerB, "/wagering/transactions/"+uuid.NewString())
	require.Equal(t, http.StatusNotFound, byID)
	require.Equal(t, http.StatusNotFound, unknown)
	require.Equal(t, bodyUnknown, bodyByID, "what exists but is not yours looks exactly like what does not exist")

	underItsOwnName, _, bodyOwn, _ := a.read(t, a.client, a.providerB, "/providers/provider-b/wagering/transactions/tx-1")
	nothing, _, bodyNothing, _ := a.read(t, a.client, a.providerB, "/providers/provider-b/wagering/transactions/tx-9999")
	require.Equal(t, http.StatusNotFound, underItsOwnName, "tx-1 is provider-a's: provider-b has no tx-1")
	require.Equal(t, http.StatusNotFound, nothing)
	require.Equal(t, bodyNothing, bodyOwn)

	underTheirName, _, _, _ := a.read(t, a.client, a.providerB, "/providers/provider-a/wagering/transactions/tx-1")
	require.Equal(t, http.StatusForbidden, underTheirName, "and it cannot ask in provider-a's namespace at all")

	opening, _, _, _ := a.read(t, a.client, a.provider, "/wagering/transactions/"+openingID)
	require.Equal(t, http.StatusNotFound, opening, "not even its own provider reaches an internal opening")

	// The owner still reads it, and nothing above changed anything.
	ownerStatus, _, _, _ := a.read(t, a.client, a.provider, "/wagering/transactions/"+mine.TransactionID)
	require.Equal(t, http.StatusOK, ownerStatus)
	require.Equal(t, before, a.tables(t), "reads, allowed or denied, leave no trace")

	// No credential, no read.
	none, _, _, _ := a.read(t, a.client, "", "/wagering/transactions/"+mine.TransactionID)
	require.Equal(t, http.StatusUnauthorized, none)
	a.client.requireNoViolations()
}

// A provider cannot learn that a key exists by trying it: its own keys and ids are its
// namespace. Replaying with the same key under its own name is a new operation; under
// another's name, a refusal.
func TestAProviderCannotReplayOrProbeAKeyItDoesNotOwn(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	_, _, first := a.submit(t, a.strict, a.provider, "shared-key", operation(w, "shared-id", "BET", "10.00"))

	asB := operation(w, "shared-id", "BET", "10.00")
	asBForA := asB // the body still names provider-a
	status, _, _ := a.submit(t, a.client, a.providerB, "shared-key", asBForA)
	require.Equal(t, http.StatusForbidden, status, "provider-b cannot replay provider-a's operation")

	status, _, answer := a.submit(t, a.strict, a.provider, "shared-key", operation(w, "shared-id", "BET", "10.00"))
	require.Equal(t, http.StatusOK, status)
	require.True(t, answer.IdempotentReplay)
	require.Equal(t, first.TransactionID, answer.TransactionID, "the owner replays it")
	balance, _ := balanceOf(t, a, w.id)
	require.Equal(t, int64(9000), balance)
}

func balanceOf(t *testing.T, a walletAPI, id string) (balance, version int64) {
	t.Helper()
	require.NoError(t, a.pool.QueryRow(t.Context(), `SELECT balance_minor, version FROM wallets WHERE id = $1`, id).Scan(&balance, &version))
	return balance, version
}

// There is no way yet to reach PENDING_REFERENCE through the API, so the row is stored
// with the repository the way the pending-reference flow will.
func TestAPendingTransactionIsReadAsPending(t *testing.T) {
	a := startWalletAPI(t)
	w := a.openFunded(t, "100.00")
	walletID := uuid.MustParse(w.id)
	playerID := uuid.MustParse(w.player)
	amount, err := money.FromMinor(1000, money.BRL)
	require.NoError(t, err)
	request, err := wager.NewRequest(wager.RequestParams{
		ProviderID: "provider-a", ExternalTransactionID: "tx-refund", PlayerID: playerID, WalletID: walletID,
		RoundID: "round-1", GameID: "game-1", Kind: wager.KindRefund, Amount: amount, ReferenceExternalTransactionID: "tx-not-here-yet",
	})
	require.NoError(t, err)
	pending, err := wager.New(wager.NewParams{ID: uuid.New(), Request: request, IdempotencyKey: "k-refund", CorrelationID: "c", At: time.Now()})
	require.NoError(t, err)
	waiting, err := pending.AwaitReference(time.Now())
	require.NoError(t, err)

	uow := postgres.NewUnitOfWork(a.pool)
	require.NoError(t, uow.Do(t.Context(), func(ctx context.Context, repos application.Repositories) error {
		_, err := repos.Transactions().Claim(ctx, waiting)
		return err
	}))

	status, _, _, tx := a.read(t, a.strict, a.provider, "/wagering/transactions/"+waiting.ID().String())
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "PENDING_REFERENCE", tx.Status)
	require.Nil(t, tx.Balance)
	require.Empty(t, tx.FailureCode)
	require.Empty(t, tx.ProcessedAt, "it has not ended")

	status, _, _, tx = a.read(t, a.strict, a.provider, "/providers/provider-a/wagering/transactions/tx-refund")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "PENDING_REFERENCE", tx.Status)
	a.strict.requireNoViolations()
}
