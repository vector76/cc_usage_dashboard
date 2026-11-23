package main

// enterpriseDetector builds the predicate that switches the dashboard into
// enterprise mode. plan is config.Plan ("auto", "enterprise" or "quota");
// fromCredentials reports whether the credentials file says enterprise and is
// consulted only for "auto", so a forced mode never touches the file.
func enterpriseDetector(plan string, fromCredentials func() bool) func() bool {
	switch plan {
	case "enterprise":
		return func() bool { return true }
	case "quota":
		return func() bool { return false }
	default:
		return fromCredentials
	}
}
