package copper

import _ "embed"

// contract codes for Mainnet upgrade
var (
	//go:embed mainnet/ValidatorContract
	MainnetValidatorContract string
	//go:embed default/StakeHub
	MainnetStakeHubContract string
)

// contract codes for Chapel upgrade
var (
	//go:embed chapel/ValidatorContract
	ChapelValidatorContract string
	//go:embed default/StakeHub
	ChapelStakeHubContract string
)

// contract codes for Default upgrade
var (
	//go:embed default/ValidatorContract
	DefaultValidatorContract string
	//go:embed default/StakeHub
	DefaultStakeHubContract string
)
