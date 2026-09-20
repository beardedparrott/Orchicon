package main

// tzdata.go — the binary carries its own timezone database, EXPLICITLY.
//
// The control plane needs IANA zone rules to interpret a recurring schedule's stored zone
// (workitem.RecurringZone → time.LoadLocation) and to render local times. It HAS been getting them
// transitively: the OPA policy engine imports time/tzdata, so the package is linked and its init
// registers the embedded fallback. That works today and is not a guarantee — it is a side effect of
// depending on OPA. If the policy engine were ever swapped or trimmed, the server would lose its zone
// database with no compile error and no test failure, and every zone lookup would fall back to UTC
// silently. A schedule stamped "America/Chicago" would then fire at 09:00 UTC and the API would look
// perfectly healthy.
//
// So it is imported HERE, in the binary that depends on it, where the dependency is visible and the
// removal would have to be deliberate. The cost is ~450 KB; the alternative is a silent correctness
// bug in the scheduler.
//
// The filesystem is still preferred, so the image's own tzdata (if present) wins and this is only the
// fallback for a deployment that ships none.
import _ "time/tzdata"
