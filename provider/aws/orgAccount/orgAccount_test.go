package orgAccount

import (
	"strings"
	"testing"

	orgtypes "github.com/aws/aws-sdk-go-v2/service/organizations/types"
)

func valid() Args {
	return Args{
		Name: "payments-prod", Email: "aws+payments-prod@acme.com", OuID: "ou-a", SuspendedOuID: "ou-s",
		OrgName: "acme", DeployerArns: []string{"arn:aws:iam::111111111111:role/deploy"},
	}
}

func TestValidate(t *testing.T) {
	if err := validate(valid()); err != nil {
		t.Fatalf("valid args: %v", err)
	}
	cases := map[string]func(*Args){
		"1-50 characters":       func(a *Args) { a.Name = strings.Repeat("a", 51) },
		"valid address":         func(a *Args) { a.Email = "not-an-email" },
		"at most 64":            func(a *Args) { a.Email = strings.Repeat("a", 60) + "@acme.com" },
		"the Suspended OU":      func(a *Args) { a.SuspendedOuID = "" },
		"at least one deployer": func(a *Args) { a.DeployerArns = nil },
	}
	for want, mutate := range cases {
		a := valid()
		mutate(&a)
		if err := validate(a); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
}

func TestCreateFailure_SaysWhatToDo(t *testing.T) {
	cases := map[orgtypes.CreateAccountFailureReason]string{
		orgtypes.CreateAccountFailureReasonEmailAlreadyExists:        "Give it another email",
		orgtypes.CreateAccountFailureReasonAccountLimitExceeded:      "Request an increase",
		orgtypes.CreateAccountFailureReasonMissingBusinessValidation: "business details",
		orgtypes.CreateAccountFailureReasonInvalidPaymentInstrument:  "payment method",
		orgtypes.CreateAccountFailureReasonInternalFailure:           "Deploy again",
	}
	for reason, want := range cases {
		if err := createFailure(valid(), reason); !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", reason, err, want)
		}
	}
}
