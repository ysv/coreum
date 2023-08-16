package integrationtests

import (
	"context"
	"testing"
	"time"

	"github.com/cosmos/cosmos-sdk/client/grpc/tmservice"
	sdk "github.com/cosmos/cosmos-sdk/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	govtypesv1beta1 "github.com/cosmos/cosmos-sdk/x/gov/types/v1beta1"
	paramproposal "github.com/cosmos/cosmos-sdk/x/params/types/proposal"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"

	"github.com/CoreumFoundation/coreum-tools/pkg/retry"
	"github.com/CoreumFoundation/coreum/v3/pkg/client"
	"github.com/CoreumFoundation/coreum/v3/testutil/event"
)

const submitProposalGas = 400_000

// Governance keep the test chain predefined account for the governance operations.
type Governance struct {
	chainCtx       ChainContext
	faucet         Faucet
	govClient      govtypesv1beta1.QueryClient
	stakerAccounts []sdk.AccAddress
	muCh           chan struct{}
}

// NewGovernance returns the new instance of Governance.
func NewGovernance(chainCtx ChainContext, stakerMnemonics []string, faucet Faucet) Governance {
	stakerAccounts := make([]sdk.AccAddress, 0, len(stakerMnemonics))
	for _, stakerMnemonic := range stakerMnemonics {
		stakerAccounts = append(stakerAccounts, chainCtx.ImportMnemonic(stakerMnemonic))
	}

	gov := Governance{
		chainCtx:       chainCtx,
		faucet:         faucet,
		stakerAccounts: stakerAccounts,
		govClient:      govtypesv1beta1.NewQueryClient(chainCtx.ClientContext),
		muCh:           make(chan struct{}, 1),
	}
	gov.muCh <- struct{}{}

	return gov
}

// ComputeProposerBalance computes the balance required for the proposer.
func (g Governance) ComputeProposerBalance(ctx context.Context) (sdk.Coin, error) {
	govParams, err := g.queryGovParams(ctx)
	if err != nil {
		return sdk.Coin{}, err
	}

	minDeposit := govParams.DepositParams.MinDeposit[0]
	return g.chainCtx.NewCoin(minDeposit.Amount.Add(g.chainCtx.ChainSettings.GasPrice.Mul(sdk.NewDec(int64(submitProposalGas))).Ceil().RoundInt())), nil
}

// UpdateParams goes through proposal process to update parameters.
func (g Governance) UpdateParams(ctx context.Context, t *testing.T, description string, updates []paramproposal.ParamChange) {
	t.Helper()
	// Fund accounts.
	proposer := g.chainCtx.GenAccount()
	proposerBalance, err := g.ComputeProposerBalance(ctx)
	require.NoError(t, err)

	g.faucet.FundAccounts(ctx, t, NewFundedAccount(proposer, proposerBalance))

	g.ProposeAndVote(ctx, t, proposer,
		paramproposal.NewParameterChangeProposal("Updating parameters", description, updates), govtypesv1beta1.OptionYes)
}

// ProposeAndVote create a new proposal, votes from all stakers accounts and awaits for the final status.
func (g Governance) ProposeAndVote(ctx context.Context, t *testing.T, proposer sdk.AccAddress, content govtypesv1beta1.Content, option govtypesv1beta1.VoteOption) {
	t.Helper()
	proposalMsg, err := g.NewMsgSubmitProposal(ctx, proposer, content)
	require.NoError(t, err)

	proposalID, err := g.Propose(ctx, t, proposalMsg)
	require.NoError(t, err)

	proposal, err := g.GetProposal(ctx, proposalID)
	require.NoError(t, err)

	if govtypesv1beta1.StatusVotingPeriod != proposal.Status {
		t.Fatalf("unexpected proposal status after creation: %s", proposal.Status)
	}

	err = g.VoteAll(ctx, option, proposal.ProposalId)
	require.NoError(t, err)

	t.Logf("Voters have voted successfully, waiting for voting period to be finished, votingEndTime:%s", proposal.VotingEndTime)

	finalStatus, err := g.WaitForVotingToFinalize(ctx, proposalID)
	require.NoError(t, err)

	if finalStatus != govtypesv1beta1.StatusPassed {
		t.Fatalf("unexpected proposal status after voting: %s, expected: %s", finalStatus, govtypesv1beta1.StatusPassed)
	}

	t.Logf("Proposal has been submitted, proposalID: %d", proposalID)
}

// Propose creates a new proposal.
func (g Governance) Propose(ctx context.Context, t *testing.T, msg *govtypesv1beta1.MsgSubmitProposal) (uint64, error) {
	SkipUnsafe(t)
	txf := g.chainCtx.TxFactory().WithGas(submitProposalGas)
	result, err := client.BroadcastTx(
		ctx,
		g.chainCtx.ClientContext.WithFromAddress(msg.GetProposer()),
		txf,
		msg,
	)
	if err != nil {
		return 0, err
	}

	proposalID, err := event.FindUint64EventAttribute(result.Events, govtypes.EventTypeSubmitProposal, govtypes.AttributeKeyProposalID)
	if err != nil {
		return 0, err
	}

	return proposalID, nil
}

// NewMsgSubmitProposal - is a helper which initializes MsgSubmitProposal with args passed and prefills min deposit.
func (g Governance) NewMsgSubmitProposal(ctx context.Context, proposer sdk.AccAddress, content govtypesv1beta1.Content) (*govtypesv1beta1.MsgSubmitProposal, error) {
	govParams, err := g.queryGovParams(ctx)
	if err != nil {
		return nil, err
	}

	msg, err := govtypesv1beta1.NewMsgSubmitProposal(content, govParams.DepositParams.MinDeposit, proposer)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return msg, nil
}

// VoteAll votes for the proposalID from all voting accounts with the provided VoteOption.
func (g Governance) VoteAll(ctx context.Context, option govtypesv1beta1.VoteOption, proposalID uint64) error {
	return g.voteAll(ctx, func(voter sdk.AccAddress) sdk.Msg {
		return &govtypesv1beta1.MsgVote{
			ProposalId: proposalID,
			Voter:      voter.String(),
			Option:     option,
		}
	})
}

// VoteAllWeighted votes for the proposalID from all voting accounts with the provided WeightedVoteOptions.
func (g Governance) VoteAllWeighted(ctx context.Context, options govtypesv1beta1.WeightedVoteOptions, proposalID uint64) error {
	return g.voteAll(ctx, func(voter sdk.AccAddress) sdk.Msg {
		return &govtypesv1beta1.MsgVoteWeighted{
			ProposalId: proposalID,
			Voter:      voter.String(),
			Options:    options,
		}
	})
}

func (g Governance) voteAll(ctx context.Context, msgFunc func(sdk.AccAddress) sdk.Msg) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.muCh:
		defer func() {
			g.muCh <- struct{}{}
		}()
	}

	txHashes := make([]string, 0, len(g.stakerAccounts))
	for _, staker := range g.stakerAccounts {
		msg := msgFunc(staker)

		txf := g.chainCtx.TxFactory().
			WithSimulateAndExecute(true)

		clientCtx := g.chainCtx.ClientContext.
			WithAwaitTx(false)

		res, err := client.BroadcastTx(ctx, clientCtx.WithFromAddress(staker), txf, msg)
		if err != nil {
			return err
		}
		txHashes = append(txHashes, res.TxHash)
	}

	// await for the first error
	for _, txHash := range txHashes {
		_, err := client.AwaitTx(ctx, g.chainCtx.ClientContext, txHash)
		if err != nil {
			return err
		}
	}

	return nil
}

// WaitForVotingToFinalize waits for the proposal status to change to final.
// Final statuses are: StatusPassed, StatusRejected or StatusFailed.
func (g Governance) WaitForVotingToFinalize(ctx context.Context, proposalID uint64) (govtypesv1beta1.ProposalStatus, error) {
	proposal, err := g.GetProposal(ctx, proposalID)
	if err != nil {
		return proposal.Status, err
	}

	tmQueryClient := tmservice.NewServiceClient(g.chainCtx.ClientContext)
	blockRes, err := tmQueryClient.GetLatestBlock(ctx, &tmservice.GetLatestBlockRequest{})
	if err != nil {
		return proposal.Status, errors.WithStack(err)
	}
	if blockRes.SdkBlock.Header.Time.Before(proposal.VotingEndTime) {
		waitCtx, waitCancel := context.WithTimeout(ctx, proposal.VotingEndTime.Sub(blockRes.SdkBlock.Header.Time))
		defer waitCancel()

		<-waitCtx.Done()
		if ctx.Err() != nil {
			return proposal.Status, ctx.Err()
		}
	}

	retryCtx, retryCancel := context.WithTimeout(ctx, 10*time.Second)
	defer retryCancel()

	err = retry.Do(retryCtx, time.Second, func() error {
		proposal, err = g.GetProposal(ctx, proposalID)
		if err != nil {
			return err
		}

		switch proposal.Status {
		case govtypesv1beta1.StatusPassed, govtypesv1beta1.StatusRejected, govtypesv1beta1.StatusFailed:
			return nil
		default:
			return retry.Retryable(errors.Errorf("waiting for one of final statuses but current one is %s", proposal.Status))
		}
	})
	if err != nil {
		return proposal.Status, err
	}
	return proposal.Status, nil
}

// GetProposal returns proposal by ID.
func (g Governance) GetProposal(ctx context.Context, proposalID uint64) (govtypesv1beta1.Proposal, error) {
	resp, err := g.govClient.Proposal(ctx, &govtypesv1beta1.QueryProposalRequest{
		ProposalId: proposalID,
	})
	if err != nil {
		return govtypesv1beta1.Proposal{}, err
	}

	return resp.Proposal, nil
}

func (g Governance) queryGovParams(ctx context.Context) (govtypesv1beta1.Params, error) {
	govClient := g.govClient

	votingParams, err := govClient.Params(ctx, &govtypesv1beta1.QueryParamsRequest{
		ParamsType: govtypesv1beta1.ParamVoting,
	})
	if err != nil {
		return govtypesv1beta1.Params{}, errors.WithStack(err)
	}

	depositParams, err := govClient.Params(ctx, &govtypesv1beta1.QueryParamsRequest{
		ParamsType: govtypesv1beta1.ParamDeposit,
	})
	if err != nil {
		return govtypesv1beta1.Params{}, errors.WithStack(err)
	}

	taillyParams, err := govClient.Params(ctx, &govtypesv1beta1.QueryParamsRequest{
		ParamsType: govtypesv1beta1.ParamTallying,
	})
	if err != nil {
		return govtypesv1beta1.Params{}, errors.WithStack(err)
	}

	return govtypesv1beta1.Params{
		VotingParams:  votingParams.VotingParams,
		DepositParams: depositParams.DepositParams,
		TallyParams:   taillyParams.TallyParams,
	}, nil
}
