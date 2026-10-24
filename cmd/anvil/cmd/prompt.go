package cmd

// defaultEnvironment is the environment a new stage gets. Anvil no longer asks
// for one; pass --environment prod to bootstrap or stage set to override it.
const defaultEnvironment = "nonprod"
