package copper_fix

import _ "embed"

// contract codes for Mainnet upgrade
var (
	//go:embed mainnet/StakeCredit
	MainnetStakeCreditContract string
	//go:embed mainnet/StakeHub
	MainnetStakeHubContract string
)

// contract codes for Chapel upgrade
var (
	//go:embed chapel/StakeCredit
	ChapelStakeCreditContract string
	//go:embed chapel/StakeHub
	ChapelStakeHubContract string
)

// contract codes for Default upgrade
var (
	//go:embed default/StakeCredit
	DefaultStakeCreditContract string
	//go:embed default/StakeHub
	DefaultStakeHubContract string
)
