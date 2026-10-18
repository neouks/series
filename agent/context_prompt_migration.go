package agent

// SHA-256 of the previous code defaults; only exact matches are upgraded.
var PreviousContextPromptHashes = map[string]string{
	"mainagent": "12bb69ee0886c4fb9beb936d882021a91104860f1da69a5e3eda3d9e345cd399",
	"worker":    "9cfe2314eccfa52e3e1fb5fb10f952e7a06a17e6f7fad3506e5b070d5569330d",
	"planner":   "d64b844d07b8dd6edd8c70e66a76369a7b977278dce571618c54a6ea2c815972",
	"auto":      "b2a71b82243607337b19f0642317fed88c144d536412d80eaaf90807f2905a1d",
	"pentest":   "2444f4e8157ef93a7c6b900b062caf79bd2a90d49fda2566c10b8b3fb24fd43f",
	"reporter":  "8803227805c947bef6bdb761ed5d60539645940f8af2a91348902ad476f944a1",
}
