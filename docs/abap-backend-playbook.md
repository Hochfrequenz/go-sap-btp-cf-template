# ABAP backend playbook

This playbook is for anyone, developer or AI agent, building a Go app on this template **together with** its own HTTP API on an on-premise ABAP system. It collects what the first such product learned the expensive way. Most of that was SAP system behaviour that no scaffolding would have prevented. The template's copy of this file is the maintained one. A fork gets a snapshot, so check the template for updates before you rely on an old copy. It complements the Go-side docs ([README](../README.md), [CLAUDE.md](../CLAUDE.md), [`btpingo`'s `doc.go`](https://pkg.go.dev/github.com/hochfrequenz/btpingo)), which cover Destinations, the Cloud Connector, XSUAA and the Go handler.

**Target syntax floor: ABAP 7.40 SP08.** Every snippet below keeps to it: inline declarations, `NEW`, `VALUE #( )`, `xsdbool( )`, string templates and `@`-escaped host variables, and nothing from 7.5x. The code ran on an ECC 6.0 system (SAP_BASIS 750) and on an S/4HANA system (SAP_BASIS 816). 7.40 is a floor we chose ourselves, below both of them. `abaplint.json` pins it, so a 7.5x construct fails the lint run before it reaches a system.

Examples use the namespace `/XYZ/` (files `#xyz#…`). With a customer namespace the same names become `Z…` (`ZCL_FOOAPI_HANDLER`). Nothing in here depends on which one you choose.

## Contents

- [1. Conventions both halves agree on](#1-conventions-both-halves-agree-on)
- [2. Pitfalls](#2-pitfalls)
- [3. Reference implementation](#3-reference-implementation)
- [4. Data-heavy endpoints (considerations)](#4-data-heavy-endpoints-considerations)

## 1. Conventions both halves agree on

The Go side of these conventions is not in the template. The template's `CallOnPremise` hands your handler the raw response, and `/healthz` is its only probe. Your fork writes the code mapping and `/readyz`. What follows is how the first product did it.

**Every non-2xx the handler answers has a JSON body `{"code":"…"}`, optionally with `"detail"`.** That covers 404, 405, every 4xx and 500. The Go half finds the ABAP code by reading the body. A body that is not JSON, or has no `code`, maps to 502 `upstream_unreachable` ("on-premise system returned HTTP n"). A code the Go build has no mapping for is also a 502, so **ship the Go mapping before the ABAP code goes live**. Go forwards `detail` to its client only for codes whose detail comes from the caller: values the caller sent, or facts the API already publishes. Codes that describe system state keep their detail in the Go log. Keep a detail to one short sentence.

**ABAP codes are internal.** Go maps every ABAP code to its own client-facing code and status, and the client-facing set is the contract. The ABAP side still sets a sensible HTTP status (404 for "not found", 5xx for its own faults), so that a proxy, a log line or someone with `curl` is not told "bad request" about a missing object. Keep the code-to-status mapping in one `CASE` in the handler. Exceptions carry the code as data, not a status.

**`sap-client` is required configuration, never a request parameter.** Without it ICF falls back to the system's default client (`login/system_client`). On a multi-client system the wrong client answers **200 with the wrong data**. Where the technical user does not exist in the default client, you get a 401 that says nothing about clients. Make the Go app refuse to start without a valid client, append `sap-client=<client>` to every on-premise path in one place (the template does not do this for you), and have `GET /system` report the client so a consumer can check it. Making the client a request parameter would need `CLIENT SPECIFIED` on every `SELECT` and turns a wrong parameter into a silent wrong answer.

**A read-only API: CSRF off on the ICF node, and every call goes through `CallOnPremise`, POST reads included.** Reads that carry a body (a filter tree, a table name) are POSTs, because the HTTP `QUERY` method is rejected by ICF, by the Connectivity proxy and by OpenAPI alike. `CallOnPremiseMutating` fetches a CSRF token first **whatever the method**, from `WithCSRFFetchPath` (default `/sap/bc/adt/discovery`). If that path is your own node, which issues no token, the helper returns an error before the request is sent, and `ClassifyOnPremError` turns it into a 502 "on-premise transport error". If you ever switch CSRF protection on at the node, ICF rejects unsafe methods before your handler runs, and the call path has to be decided again. A write API is a separate design decision, not a switch to the other helper. If you do use `CallOnPremiseMutating`, point `btp.WithCSRFFetchPath` at a GET on your own node **with `sap-client`**. Otherwise the fetch authenticates against the default client: it 401s where the technical user does not exist there, and a token from another client is not valid for yours.

**`GET /system` is the readiness target.** It is the cheapest GET on the node and answers `{"sid":"…","client":"…","release":"…"}`, all strings. Go's `/readyz` probes it with its own short deadline (about 10 s, not the query deadline), caches the result for about 30 s, and answers a coarse `{"status","upstream"}` only: no SID, no hostname, no error text. CF's platform health check stays on `/healthz`. A health check that depends on SAP turns a Cloud Connector blip into an app restart.

**Timeouts are Go context deadlines.** ABAP has no cooperative cancellation point inside a running Open SQL statement. When Go gives up, the work process runs to completion or until the system's maximum runtime (`rdisp/max_wprun_time`; newer kernels replace it with the `rdisp/scheduler/prio_*/max_runtime` parameters, so check which one your system uses) ends it with a `TIME_OUT` dump. A client that retries a 504 without backoff piles up work processes on the SAP system. Document backoff on 504 for your API's clients, and set the deadline per deployment, because a legacy system may need a longer leash than a newer one.

**Decide where authorization lives, and write the decision down.** The first product put all of it in Go: XSUAA scopes and a per-deployment switch for content reads. It had no `AUTHORITY-CHECK` in ABAP, no `S_ICF` on the node, and Open SQL performs no table authorization check of its own. The Cloud Connector path allow-list was the only SAP-side control, and the API could reach everything the technical user can reach. That was right for a read-only API behind one technical user with trusted callers. It is not automatically right for yours. If you keep it, mark in the code where an `AUTHORITY-CHECK` would go.

**Every new ABAP path is several edits in two repositories:**

1. Go: a path constant and the endpoint that calls it.
2. ABAP: the per-path allowed-methods table **and** the dispatch `CASE`. A path in only one of them answers 404 `path_not_found`, which reads like a SICF problem rather than missing code.
3. The Cloud Connector allow-list, if its entries are per path rather than one prefix. A path the allow-list does not cover never reaches SAP; it arrives as a non-JSON answer, which Go maps to 502, as if the whole system were down.

**Planned: `X-Request-Id` forwarded and echoed.** Go sends its request ID on the on-premise call, and the ABAP handler validates it, echoes it on every response it writes and records it with every failure. Neither half of the first product does this on its released version yet: forwarding on the Go side is in review, the ABAP side is not started. Both are tracked in #132. ICF's own 401/403 never reach the handler, so they cannot carry the ID.

## 2. Pitfalls

Each entry starts with what you see, then the cause, then what to do or check.

### ICF and abapGit

**The endpoint answers 403 after an abapGit pull that reported success.**
Cause: an abapGit pull activates an ICF node only when it **creates** the node. On a re-pull, `CL_ICF_TREE=>CHANGE_NODE` writes a junk row instead of setting the node's active flag. So a node deactivated by hand, by a transport or by a system copy stays inactive.
Do: reactivate the node in SICF by hand. Check with `SELECT icfactive FROM icfservloc WHERE icf_name = '<NODE>'` (`X` means active). Pulling again does not help.

**The node works on one system, and after a pull onto another system it authenticates differently.**
Cause: the node's logon settings are not serialized. abapGit carries the node and its handler class, not its authentication settings.
Do: set logon data in SICF by hand on every system and write down what you set.

**A child SICF node lands one level too high, silently.**
Cause: abapGit deserializes SICF nodes in object-name order, not tree order. A child whose name sorts before its parent's is created before the parent exists. With a parent `xyz` and a child `fooapi` the child sorts first, which is exactly the failing case.
Do: before adding or renaming a node, compare the names. After the first pull, check the path in SICF.

**One package produces different files on two systems, or a pull fails on one system only.**
Cause: the installed abapGit versions differ between systems. XML from a newer serializer can carry attributes an older one rejects.
Do: compare abapGit versions on every system before the first round trip, and again after updating one of them.

**A pull fails, or leaves objects in an odd state, after an XML edit.**
Cause: hand-written or hand-edited abapGit XML. SICF file names embed a hash, which is also why renaming a namespace is not a search-and-replace.
Do: never write abapGit XML by hand. Create the object in SAP and let abapGit serialize it, or copy an existing file of the same object type from the same repo.

**Diagnose by status code.** 401 and 403 come from ICF before your handler runs, which is why they carry no JSON body:

| Status                                    | Means                                                                                                                                                                                                  | Check                                                                                              |
| ----------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------- |
| 404                                       | wrong path: the node does not exist, or the handler does not know the sub-path. A body `{"code":"path_not_found"}` means your handler ran, so the node is fine and the path is missing from its tables | SICF tree; the handler's allowed-methods table and `CASE`                                          |
| 401                                       | the node is **active** and wants credentials: wrong user or password, or a user that does not exist in the client used (a missing `sap-client`)                                                        | Destination user; `sap-client` on the call                                                         |
| 403                                       | the node **exists but is inactive**                                                                                                                                                                    | `icfservloc-icfactive`; reactivate in SICF                                                         |
| 405                                       | the handler ran; the path exists but not with this method. The `Allow` header names the accepted ones                                                                                                  | Go's method for that path                                                                          |
| 502 from Go (non-2xx or transport detail) | the call may never have reached SAP: the Cloud Connector refuses a path outside its allow-list (expected to show as the connector's own non-JSON 403, not measured), or the system is down             | allow-list entry for the path; the body text in the Go log (ICF page vs connector page); `/readyz` |
| 500 with an HTML body (Go: 502)           | a short dump                                                                                                                                                                                           | ST22                                                                                               |

Call the node directly with `curl -u '<user>:<password>' 'https://<host>:<port>/sap/bc/rest/xyz/fooapi/system?sap-client=<client>'` to take BTP out of the picture. Check the `client` field in the answer.

### Activation and tests

**ADT reports that activation succeeded, and nothing was activated.**
Cause: activating a class through ADT with only the class URI does nothing on some systems, for example transportable classes. The changed method, definition and test-include sub-objects have to be passed as well.
Do: pass every entry the inactive-objects list shows (the changed method rows and the section rows), then read the inactive-objects list again. If the class is still listed, activate it in SE24 (Ctrl+F3, select every row in the popup). An abapGit pull can leave objects inactive while reporting success in the same way, and SA38 then happily runs the **old** version.

**The test run says `Passed: 0, Failed: 0`.**
Cause: nothing ran, so this is not a pass. Known reasons: a class pool that fails to load (a `STRING` used as a table-key component does this, even after a clean activation), a stale test include, a `RISK LEVEL` or `DURATION` the run profile excludes, or a test class the runner never found.
Do: read `Passed: N` against the number of tests you expect. Re-activate a stale include through SE24's local test classes.

**The full suite is green against source you deliberately broke.**
Cause: the upload never activated (see above), so the tests ran against the previous, correct code. This happened in a mutation run: every test green against deliberately broken source.
Do: confirm activation before you believe a test result, and state which system the result came from.

**The result looks plausible but came from the other system.**
Cause: the ADT and SAP GUI tooling keep separate system selections, and a selection can silently revert (a reconnect is enough).
Do: before activating, running tests you will report, or measuring anything, run `SELECT component, release FROM cvers WHERE component = 'SAP_BASIS'`. Two systems on different releases answer differently.

**CI is green, and the code fails to compile on SAP.**
Cause: abaplint parses; with this config it neither executes nor type-checks. Defects that reached SAP through a clean abaplint run include a 34-character method name, a `'…'` literal where a `string` table row needed backticks, and a missing `RAISING` clause.
Do: treat a lint run as a lint run. A test has run only when it has run on a real system. Report counts and the system, for example "35/35 green on the ECC system".

### Syntax floor and compile traps

**A name longer than 30 characters is a hard compile error.** abaplint has no dedicated rule for this. The `forbidden_identifier` regex in the [`abaplint.json`](#abaplintjson) below closes the gap. Test method names are the usual offenders.

**Trailing blanks disappear from a `string`.** A `'…'` literal is type `c`, and converting `c` to `string` drops trailing blanks. Use backtick literals (`` `trailing space ` ``) wherever the value is a `string`, in `VALUE #( )` table rows in particular.

**Leading and repeated blanks vanish from caller text.** `condense( )` removes leading blanks as well as trailing ones, and collapses every inner run of blanks down to one. Caller-supplied text can legitimately carry leading or repeated blanks, and `condense( )` throws those away with no warning. Never reach for it to trim caller data; strip only what you mean to strip.

**Offset access past the end of a `string` raises.** `lv(1)` on an empty `string` (or any offset/length beyond `strlen( lv )`) raises `cx_sy_range_out_of_bounds`. It is catchable, but easy to hit by surprise on caller-supplied text that turned out empty or short. Check `strlen( lv )` before the access.

**A negative number renders with the sign on the wrong side.** Converting a negative number into a character field the plain way puts the sign on the right (`5-`), not the left. The [JSON writer](#json-writer-sxml)'s `write_number` already covers this: a string template (`|{ iv_value }|`) gives `-5`, a plain assignment gives `5-`. Use a string template wherever you render a number as text.

**A malformed request body answers 500 instead of 400.** Every `CX_SXML_*` exception is a `CX_DYNAMIC_CHECK`, so it needs no `RAISING` clause and escapes silently to the handler's `CATCH cx_root`. That branch blames your code for the caller's bytes. At the parser boundary, catch the superclass `cx_sxml_error` (not `cx_sxml_parse_error` alone) and re-raise as your own 400 code with `io_previous`. A truncated body such as `{"table":` was once observed raising something outside the `CX_SXML_*` hierarchy on SAP_BASIS 750. A recursive-descent parser that calls the reader in many places therefore catches `cx_root`, at the cost of a real parser bug also presenting as 400, with the cause kept in `previous`.

**Lowercase keywords in global class definitions.** SAP's Class Builder regenerates the DEFINITION section of every global class in its own style (lowercase keywords, uppercase identifiers) on activation. Enforcing `keyword_case` would churn forever, so it is off in `abaplint.json` on purpose.

**No classrun in a package that must compile on two releases.** The `IF_OO_ADT_CLASSRUN~main` parameter type differs: `if_oo_adt_intrnl_classrun` on SAP_BASIS 750, `if_oo_adt_classrun_out` on 816. Anything executable goes into a plain `REPORT`.

**A dynamic `WHERE` passed as a table of `string` fails.** A dynamic condition's row type must be flat and character-like. Use a table of `c LENGTH 255` lines.

### Runtime

**A defect in ABAP shows up in Go as "upstream unreachable".**
Cause: an uncaught exception short-dumps the work process, and ICF answers with an HTML error page. Go finds no `code` in HTML and reports the Cloud Connector path as broken.
Do: catch `cx_root` at the handler boundary and answer 500 `{"code":"internal_error"}` (see the [handler skeleton](#handler-skeleton)). Go then sees a code and knows the two halves are talking. A caught exception leaves no ST22 entry, so record what you need (code, detail, `previous->get_text( )`) yourself. Only uncatchable errors still dump and still answer HTML, for example `TIME_OUT`, memory exhaustion (`TSV_TNEW_PAGE_ALLOC_FAILED`, also what runaway recursion ends in), a failed `ASSERT`, a conversion exit's `CONV_EXIT_FIELD_TOO_SHORT`, or an unhandled classic function-module exception. Large `SELECT`s are where the first two bite.

**A deeply nested request body takes down the work process instead of failing with 400.**
Cause: ABAP raises no catchable exception for recursion that runs too deep; the session exhausts its memory and short-dumps. A recursive parser that descends once per nesting level of a caller-supplied structure — nested JSON, a tree of filter conditions — does exactly that on a deeply nested enough body, taking the work process down: an HTML 500 (see above) for a request body, which Go and anything watching the system reads as a fault rather than a bad request. A limit on the parsed result's node count or size does not help here: by the time that check runs, the tree is already built and the recursion has already happened.
Do: give the parser an explicit maximum nesting depth and check it **before** each recursive call, not on the result afterwards. Add a node-count limit alongside it for breadth; a depth limit alone does not bound a wide-but-shallow body.

**A conversion exit takes down the work process instead of raising an exception.**
Cause: `CONVERSION_EXIT_ALPHA_INPUT` (and similar exits) writes into the caller's output field, and an over-long input value raises the non-catchable runtime error `CONV_EXIT_FIELD_TOO_SHORT`, not a `CX_` class. A length guard placed after the call never runs — the kernel aborts first.
Do: validate the input's length **before** calling the conversion exit, not after.

### Optional add-ons

**The whole package fails to activate on a system without some add-on.**
Cause: a static reference to one of the add-on's DDIC objects. One `DATA lv TYPE <addon data element>` is enough, not only a table reference.
Do: never name an optional add-on's DDIC objects statically, not even its data elements. Keep the table names as character constants, access them dynamically, and put a presence seam in front:

```abap
INTERFACE /xyz/if_fooapi_presence PUBLIC.
  METHODS is_present
    IMPORTING iv_tabname        TYPE clike
    RETURNING VALUE(rv_present) TYPE abap_bool.
ENDINTERFACE.

" Production implementation: DD02L, active version only.
METHOD /xyz/if_fooapi_presence~is_present.
  DATA lv_tabname  TYPE dd02l-tabname.
  DATA lv_tabclass TYPE dd02l-tabclass.

  lv_tabname = to_upper( iv_tabname ).
  SELECT SINGLE tabclass FROM dd02l
    INTO @lv_tabclass
    WHERE tabname  = @lv_tabname
      AND as4local = 'A'.
  rv_present = xsdbool( sy-subrc = 0 AND lv_tabclass IS NOT INITIAL ).
ENDMETHOD.
```

Inject the interface through the constructor (defaulting to the class itself), so a test double covers the absent branch on systems that all have the add-on. Use DD02L with `AS4LOCAL = 'A'`, not ADT's object-existence probe, which gives false negatives for namespaced DDIC objects. **Absence is not emptiness.** An add-on that is not installed raises its own code (the first product raised `optional_source_absent`, answered 501, and Go mapped it to its own 501 `not_implemented`: not 404, which sends people looking for a typo, and not 503, because there is nothing to retry). An add-on that is installed but has no data answers 200 with an empty list. Map the new code explicitly on both halves. The ABAP `WHEN OTHERS` answers 400 and would blame the caller.

### Two halves, two release moments

**The API's behaviour changed although nothing was released.**
Cause: the ABAP half goes live on **activation**, with no merge gate or release in between. The Go half goes live on a **release**. The first product found this when a deployment served a fix whose ABAP pull request was still open, because the class had been activated during development.
Do: treat activation on a connected system as a deploy. Order changes so that each half tolerates the other's old state:

- **New ABAP error code:** release the Go mapping first. Until then the code is a 502.
- **New path:** Go learning the path first is safe if Go maps `path_not_found` to a clean 404.
- **Several systems:** the ABAP deployments move independently (one on activation, the others on an abapGit pull or a transport). Until all are pulled they run different ABAP, and a green end-to-end test against one system measures that system's current activation state, not a released artifact.
- **Deliberately broken activation for a test:** revert it afterwards. A test system stays broken until you do.

## 3. Reference implementation

These are the pieces worth copying. Rename `FOOAPI` and `/XYZ/` and they compile on their own. The exception class comes first, because everything else raises it.

### Exception class

```abap
CLASS /xyz/cx_fooapi DEFINITION
  PUBLIC
  INHERITING FROM cx_static_check
  FINAL
  CREATE PUBLIC.

  PUBLIC SECTION.
    " Internal codes. Go maps every one to its own client-facing code.
    CONSTANTS c_not_found     TYPE string VALUE 'not_found' ##NO_TEXT.
    CONSTANTS c_invalid_value TYPE string VALUE 'invalid_value' ##NO_TEXT.
    CONSTANTS c_internal      TYPE string VALUE 'internal_error' ##NO_TEXT.

    DATA code   TYPE string READ-ONLY.
    DATA detail TYPE string READ-ONLY.

    " Always pass io_previous when wrapping a kernel exception, or the
    " root cause is gone from your log line and the debugger.
    METHODS constructor
      IMPORTING iv_code     TYPE string
                iv_detail   TYPE string OPTIONAL
                io_previous TYPE REF TO cx_root OPTIONAL.
ENDCLASS.

CLASS /xyz/cx_fooapi IMPLEMENTATION.
  METHOD constructor ##ADT_SUPPRESS_GENERATION.
    super->constructor( previous = io_previous ).
    code   = iv_code.
    detail = iv_detail.
  ENDMETHOD.
ENDCLASS.
```

### JSON writer (sXML)

A thin wrapper over `cl_sxml_string_writer` with `if_sxml=>co_xt_json`. In sXML's JSON model the element name **is** the JSON type (`object`, `array`, `str`, `num`, `bool`, `null`), and the key is a `name` attribute. The kernel does all string escaping (`"` becomes `\"`, `\` becomes `\\`, tab becomes `\t`, newline becomes `\n`; umlauts stay literal UTF-8) and emits UTF-8 without a BOM.

Alternatives we rejected:

- `CALL TRANSFORMATION id`: cannot emit a bare `null` for an initial field, and emits real numbers.
- `/ui2/cl_json`: guesses types by regex, and support packages patch it separately on each system.
- String concatenation: means writing your own escaping, which is the one part with real correctness risk.

```abap
CLASS /xyz/cl_fooapi_json DEFINITION
  PUBLIC
  FINAL
  CREATE PUBLIC.

  PUBLIC SECTION.
    METHODS constructor.

    METHODS open_object
      IMPORTING iv_name TYPE string OPTIONAL.
    METHODS close_object.
    METHODS open_array
      IMPORTING iv_name TYPE string OPTIONAL.
    METHODS close_array.

    " 'str' is always quoted, whatever the content looks like.
    METHODS write_string
      IMPORTING iv_name  TYPE string OPTIONAL
                iv_value TYPE string.
    " A bare, unquoted null.
    METHODS write_null
      IMPORTING iv_name TYPE string OPTIONAL.
    METHODS write_bool
      IMPORTING iv_name  TYPE string OPTIONAL
                iv_value TYPE abap_bool.
    METHODS write_number
      IMPORTING iv_name  TYPE string OPTIONAL
                iv_value TYPE i.

    " The raw UTF-8 bytes. The handler sends these with set_data( ).
    METHODS get_output
      RETURNING VALUE(rv_xstring) TYPE xstring.

  PRIVATE SECTION.
    " Decoded string, for tests only (reached via LOCAL FRIENDS).
    " Private so production code cannot take the string path.
    METHODS get_json
      RETURNING VALUE(rv_json) TYPE string.

    DATA mo_writer TYPE REF TO if_sxml_writer.
    DATA mo_out    TYPE REF TO cl_sxml_string_writer.
ENDCLASS.

CLASS /xyz/cl_fooapi_json IMPLEMENTATION.

  METHOD constructor.
    mo_out    = cl_sxml_string_writer=>create( type = if_sxml=>co_xt_json ).
    mo_writer = CAST if_sxml_writer( mo_out ).
  ENDMETHOD.

  METHOD open_object.
    mo_writer->open_element( name = 'object' ).
    IF iv_name IS NOT INITIAL.
      mo_writer->write_attribute( name = 'name' value = iv_name ).
    ENDIF.
  ENDMETHOD.

  METHOD close_object.
    mo_writer->close_element( ).
  ENDMETHOD.

  METHOD open_array.
    mo_writer->open_element( name = 'array' ).
    IF iv_name IS NOT INITIAL.
      mo_writer->write_attribute( name = 'name' value = iv_name ).
    ENDIF.
  ENDMETHOD.

  METHOD close_array.
    mo_writer->close_element( ).
  ENDMETHOD.

  METHOD write_string.
    mo_writer->open_element( name = 'str' ).
    IF iv_name IS NOT INITIAL.
      mo_writer->write_attribute( name = 'name' value = iv_name ).
    ENDIF.
    mo_writer->write_value( value = iv_value ).
    mo_writer->close_element( ).
  ENDMETHOD.

  METHOD write_null.
    mo_writer->open_element( name = 'null' ).
    IF iv_name IS NOT INITIAL.
      mo_writer->write_attribute( name = 'name' value = iv_name ).
    ENDIF.
    mo_writer->close_element( ).
  ENDMETHOD.

  METHOD write_bool.
    " Spelled out: ABAP's 'X'/' ' is not JSON's true/false.
    mo_writer->open_element( name = 'bool' ).
    IF iv_name IS NOT INITIAL.
      mo_writer->write_attribute( name = 'name' value = iv_name ).
    ENDIF.
    IF iv_value = abap_true.
      mo_writer->write_value( value = 'true' ).
    ELSE.
      mo_writer->write_value( value = 'false' ).
    ENDIF.
    mo_writer->close_element( ).
  ENDMETHOD.

  METHOD write_number.
    " The string template gives -5; a plain assignment of an i would
    " give '5-' (sign on the right), which is not a JSON number.
    DATA lv_text TYPE string.
    lv_text = |{ iv_value }|.
    mo_writer->open_element( name = 'num' ).
    IF iv_name IS NOT INITIAL.
      mo_writer->write_attribute( name = 'name' value = iv_name ).
    ENDIF.
    mo_writer->write_value( value = lv_text ).
    mo_writer->close_element( ).
  ENDMETHOD.

  METHOD get_output.
    rv_xstring = mo_out->get_output( ).
  ENDMETHOD.

  METHOD get_json.
    rv_json = cl_abap_codepage=>convert_from( mo_out->get_output( ) ).
  ENDMETHOD.

ENDCLASS.
```

Key order is call order, so no RTTI or hashed table decides it. For requests, `cl_sxml_string_reader` pull-parses the same vocabulary, and every scalar, numbers included, arrives as text that you validate yourself. Pin the bytes with a test. It also catches a BOM or a codepage change that a test on the decoded string cannot see:

```abap
CLASS ltcl_json DEFINITION DEFERRED.
CLASS /xyz/cl_fooapi_json DEFINITION LOCAL FRIENDS ltcl_json.

CLASS ltcl_json DEFINITION FOR TESTING
  RISK LEVEL HARMLESS
  DURATION SHORT.
  PRIVATE SECTION.
    METHODS output_bytes_are_utf8 FOR TESTING RAISING cx_static_check.
    METHODS null_is_unquoted      FOR TESTING RAISING cx_static_check.
ENDCLASS.

CLASS ltcl_json IMPLEMENTATION.

  METHOD output_bytes_are_utf8.
    " {"A":"<U+00E4>"} as UTF-8, no BOM. The umlaut comes from its code
    " point so the test does not depend on the source file's encoding.
    DATA lv_expected TYPE xstring VALUE '7B2241223A22C3A4227D'.
    DATA(lo) = NEW /xyz/cl_fooapi_json( ).
    lo->open_object( ).
    lo->write_string( iv_name  = 'A'
                      iv_value = cl_abap_conv_in_ce=>uccp( '00E4' ) ).
    lo->close_object( ).
    cl_abap_unit_assert=>assert_equals( act = lo->get_output( )
                                        exp = lv_expected ).
  ENDMETHOD.

  METHOD null_is_unquoted.
    DATA(lo) = NEW /xyz/cl_fooapi_json( ).
    lo->open_object( ).
    lo->write_null( 'X' ).
    lo->close_object( ).
    cl_abap_unit_assert=>assert_equals( act = lo->get_json( )
                                        exp = '{"X":null}' ).
  ENDMETHOD.

ENDCLASS.
```

### Reading JSON with sXML

`cl_sxml_string_reader` pull-parses the same vocabulary the writer above emits, and it has two traps of its own.

**A typed element reads back as `'name'` for every member, whatever its real JSON type.**
Cause: the element name **is** the JSON type (`str`, `num`, `object`, …), exactly as on the writing side, but `next_attribute( )` overwrites the reader's `->name` with the attribute's own name. Reading `->name` after looping the element's attributes instead of before it returns the string `'name'` for every object member (array items carry no attribute and read correctly) — it type-checks and is wrong for every member.
Do: read `io_reader->name` immediately after `co_nt_element_open`, before looping `next_attribute( )`.

**A long string value comes back truncated, with no error.**
Cause: the kernel is free to hand one scalar's text over in several `co_nt_value` nodes — escapes and long strings are the usual reasons. Assigning the value on each node keeps only the last chunk: a silently truncated value that matches different rows instead of failing.
Do: accumulate with `&&`, never assign:

```abap
DO.
  " ... read one node into lv_node_type / lv_value ...
  CASE lv_node_type.
    WHEN if_sxml_node=>co_nt_value.
      rv_value = rv_value && lv_value.
    WHEN if_sxml_node=>co_nt_element_close OR if_sxml_node=>co_nt_final.
      RETURN.
  ENDCASE.
ENDDO.
```

### Handler skeleton

The order is fixed: **404, then 405, then dispatch, then catch.** An unknown path answers 404 whatever the method, because a 405 for a mistyped URL points the client at its method instead of its URL. A known path with the wrong method answers 405 and names the accepted methods in `Allow`. Every branch sets the content type **before** the body. Successful answers go out as bytes with `set_data( )`. `set_cdata( )` would convert through whatever encoding the response carries at that moment, and that can mangle non-ASCII text. Read request bodies with `get_data( )` for the same reason.

```abap
CLASS /xyz/cl_fooapi_handler DEFINITION
  PUBLIC
  FINAL
  CREATE PUBLIC.

  PUBLIC SECTION.
    INTERFACES if_http_extension.

  PRIVATE SECTION.
    CONSTANTS c_json TYPE string VALUE 'application/json; charset=utf-8'.

    METHODS allowed_methods
      IMPORTING iv_path         TYPE string
      RETURNING VALUE(rv_allow) TYPE string.
    METHODS method_is_allowed
      IMPORTING iv_allow          TYPE string
                iv_method         TYPE string
      RETURNING VALUE(rv_allowed) TYPE abap_bool.
    METHODS status_for
      IMPORTING iv_code          TYPE string
      RETURNING VALUE(rv_status) TYPE i.
    METHODS reason_for
      IMPORTING iv_status        TYPE i
      RETURNING VALUE(rv_reason) TYPE string.
    METHODS error_body
      IMPORTING ix_error       TYPE REF TO /xyz/cx_fooapi
      RETURNING VALUE(rv_body) TYPE xstring.
    METHODS system_payload
      RETURNING VALUE(rv_body) TYPE xstring.
    METHODS things_payload
      IMPORTING iv_body        TYPE xstring
      RETURNING VALUE(rv_body) TYPE xstring
      RAISING   /xyz/cx_fooapi.
ENDCLASS.

CLASS /xyz/cl_fooapi_handler IMPLEMENTATION.

  METHOD if_http_extension~handle_request.
    DATA(lv_path)   = server->request->get_header_field( '~path_info' ).
    DATA(lv_method) = server->request->get_header_field( '~request_method' ).
    DATA(lv_allow)  = allowed_methods( lv_path ).

    " 1. Unknown path: 404, whatever the method. With a body: a bare 404
    "    would reach Go as 502 and look like a network problem.
    IF lv_allow IS INITIAL.
      server->response->set_content_type( c_json ).
      server->response->set_status( code = 404 reason = 'Not Found' ).
      server->response->set_cdata( '{"code":"path_not_found"}' ).
      RETURN.
    ENDIF.

    " 2. Known path, wrong method: 405 plus Allow.
    IF method_is_allowed( iv_allow  = lv_allow
                          iv_method = lv_method ) = abap_false.
      server->response->set_content_type( c_json ).
      server->response->set_status( code = 405 reason = 'Method Not Allowed' ).
      server->response->set_header_field( name = 'Allow' value = lv_allow ).
      server->response->set_cdata( '{"code":"method_not_allowed"}' ).
      RETURN.
    ENDIF.

    " 3. Dispatch. Every path here must also be in allowed_methods( ).
    TRY.
        CASE lv_path.
          WHEN '/system'.
            server->response->set_content_type( c_json ).
            server->response->set_data( system_payload( ) ).
            server->response->set_status( code = 200 reason = 'OK' ).

          WHEN '/things'.
            DATA(lv_out) = things_payload( server->request->get_data( ) ).
            server->response->set_content_type( c_json ).
            server->response->set_data( lv_out ).
            server->response->set_status( code = 200 reason = 'OK' ).

          WHEN OTHERS.
            " Only reachable if allowed_methods( ) knows a path this CASE
            " does not. A clean 404 rather than a 200 with no body.
            server->response->set_content_type( c_json ).
            server->response->set_status( code = 404 reason = 'Not Found' ).
            server->response->set_cdata( '{"code":"path_not_found"}' ).
        ENDCASE.

      " 4. Catch. Our own exception: its code picks the status.
      CATCH /xyz/cx_fooapi INTO DATA(lx_api).
        DATA(lv_status) = status_for( lx_api->code ).
        server->response->set_content_type( c_json ).
        server->response->set_status( code   = lv_status
                                      reason = reason_for( lv_status ) ).
        server->response->set_data( error_body( lx_api ) ).

      " Anything else is our bug. Uncaught, it dumps and ICF serves HTML.
      " The CX_SXML_* exceptions are CX_DYNAMIC_CHECK and end up here too.
      CATCH cx_root.
        " Record code, get_text( ) and previous here (application log or
        " your own table): a caught exception leaves no ST22 entry.
        server->response->set_content_type( c_json ).
        server->response->set_status( code = 500 reason = 'Internal Server Error' ).
        server->response->set_cdata( '{"code":"internal_error"}' ).
    ENDTRY.
  ENDMETHOD.

  METHOD allowed_methods.
    " One verb per path. POST for reads that carry a body.
    CASE iv_path.
      WHEN '/system'.
        rv_allow = 'GET'.
      WHEN '/things'.
        rv_allow = 'POST'.
      WHEN OTHERS.
        CLEAR rv_allow.
    ENDCASE.
  ENDMETHOD.

  METHOD method_is_allowed.
    " Exact comparison, never CS: 'POST' CS 'OS' is true. A path that
    " needs two methods has to split the list here on purpose.
    rv_allowed = xsdbool( iv_method IS NOT INITIAL AND iv_allow = iv_method ).
  ENDMETHOD.

  METHOD status_for.
    " Name every code. WHEN OTHERS is 400, so a forgotten code tells the
    " caller the request was bad.
    CASE iv_code.
      WHEN /xyz/cx_fooapi=>c_not_found.
        rv_status = 404.
      WHEN /xyz/cx_fooapi=>c_invalid_value.
        rv_status = 400.
      WHEN /xyz/cx_fooapi=>c_internal.
        rv_status = 500.
      WHEN OTHERS.
        rv_status = 400.
    ENDCASE.
  ENDMETHOD.

  METHOD reason_for.
    CASE iv_status.
      WHEN 404.
        rv_reason = 'Not Found'.
      WHEN 422.
        rv_reason = 'Unprocessable Entity'.
      WHEN 500.
        rv_reason = 'Internal Server Error'.
      WHEN 501.
        rv_reason = 'Not Implemented'.
      WHEN OTHERS.
        rv_reason = 'Bad Request'.
    ENDCASE.
  ENDMETHOD.

  METHOD error_body.
    " {"code":"...","detail":"..."}. The writer escapes the detail. If
    " the writer itself fails, fall back to the code alone, which is one
    " of our own ASCII constants.
    TRY.
        DATA(lo_json) = NEW /xyz/cl_fooapi_json( ).
        lo_json->open_object( ).
        lo_json->write_string( iv_name = 'code' iv_value = ix_error->code ).
        IF ix_error->detail IS NOT INITIAL.
          lo_json->write_string( iv_name = 'detail' iv_value = ix_error->detail ).
        ENDIF.
        lo_json->close_object( ).
        rv_body = lo_json->get_output( ).
      CATCH cx_root.
        rv_body = cl_abap_codepage=>convert_to( |\{"code":"{ ix_error->code }"\}| ).
    ENDTRY.
  ENDMETHOD.

  METHOD system_payload.
    DATA(lo_json) = NEW /xyz/cl_fooapi_json( ).
    lo_json->open_object( ).
    lo_json->write_string( iv_name = 'sid'     iv_value = CONV string( sy-sysid ) ).
    lo_json->write_string( iv_name = 'client'  iv_value = CONV string( sy-mandt ) ).
    lo_json->write_string( iv_name = 'release' iv_value = CONV string( sy-saprl ) ).
    lo_json->close_object( ).
    rv_body = lo_json->get_output( ).
  ENDMETHOD.

  METHOD things_payload.
    " Parse iv_body with cl_sxml_string_reader, do the work, serialize.
    " At the parser boundary:
    "   CATCH cx_sxml_error INTO DATA(lx_parse).
    "     RAISE EXCEPTION TYPE /xyz/cx_fooapi
    "       EXPORTING iv_code     = /xyz/cx_fooapi=>c_invalid_value
    "                 iv_detail   = 'request body is not JSON'
    "                 io_previous = lx_parse.
    RAISE EXCEPTION TYPE /xyz/cx_fooapi
      EXPORTING iv_code = /xyz/cx_fooapi=>c_internal.
  ENDMETHOD.

ENDCLASS.
```

The first product's handler wrote `{"code"}` only and dropped `detail`. The `error_body( )` above adds it, so test it before you rely on it, and keep a detail free of anything the caller did not send.

Register the class as the handler of the SICF node (`/sap/bc/rest/xyz/fooapi`). With these example names the child `fooapi` sorts before its parent `xyz`, which is the failing case under the SICF ordering pitfall above: either keep the parent node out of the package (create `/sap/bc/rest/xyz` once by hand on each system), pick a child name that sorts after the parent's, or, with a `Z` namespace, hang `zfooapi` directly under `/sap/bc/rest`. Dispatch sub-paths from `~path_info` in the handler rather than creating one SICF node per path. Adding a path then costs the handler, the allow-list and the Go client, rather than a new node and Destination as well.

### `IF_HTTP_SERVER` test double

`handle_request( )` touches exactly two members of `IF_HTTP_SERVER`: `request` and `response`. The double fills them with SAP's own `CL_HTTP_REQUEST` and `CL_HTTP_RESPONSE`, the classes ICF hands the real handler, so headers, status and body behave as in production. `add_c_msg = 1` is required. Without it the entity has no message object, every body reads as empty, and every test passes or fails for the wrong reason. The remaining interface methods are empty, because ABAP requires every interface method to be implemented. The list below is the one on the releases it was written for. If your release's `IF_HTTP_SERVER` differs, the syntax check names the methods to add or drop.

```abap
CLASS ltcl_server DEFINITION FINAL CREATE PUBLIC.
  PUBLIC SECTION.
    INTERFACES if_http_server.
    METHODS constructor
      IMPORTING iv_path   TYPE string
                iv_method TYPE string
                iv_body   TYPE string OPTIONAL.
    METHODS body
      RETURNING VALUE(rv_body) TYPE string.
    METHODS status
      RETURNING VALUE(rv_code) TYPE i.
    METHODS reason
      RETURNING VALUE(rv_reason) TYPE string.
ENDCLASS.

CLASS ltcl_server IMPLEMENTATION.

  METHOD constructor.
    CREATE OBJECT me->if_http_server~request  TYPE cl_http_request  EXPORTING add_c_msg = 1.
    CREATE OBJECT me->if_http_server~response TYPE cl_http_response EXPORTING add_c_msg = 1.
    " The two header fields the handler dispatches on, spelled as ICF does.
    me->if_http_server~request->set_header_field( name = '~path_info'      value = iv_path ).
    me->if_http_server~request->set_header_field( name = '~request_method' value = iv_method ).
    IF iv_body IS NOT INITIAL.
      " Bytes in, because the handler reads bytes.
      me->if_http_server~request->set_data( cl_abap_codepage=>convert_to( iv_body ) ).
    ENDIF.
  ENDMETHOD.

  METHOD body.
    rv_body = cl_abap_codepage=>convert_from( if_http_server~response->get_data( ) ).
  ENDMETHOD.

  METHOD status.
    if_http_server~response->get_status( IMPORTING code = rv_code ).
  ENDMETHOD.

  METHOD reason.
    if_http_server~response->get_status( IMPORTING reason = rv_reason ).
  ENDMETHOD.

  " Interface ballast: never called by the handler.
  METHOD if_http_server~append_field_url.
  ENDMETHOD.
  METHOD if_http_server~create_abs_url.
  ENDMETHOD.
  METHOD if_http_server~create_rel_url.
  ENDMETHOD.
  METHOD if_http_server~decode_base64.
  ENDMETHOD.
  METHOD if_http_server~enable_foreign_session_access.
  ENDMETHOD.
  METHOD if_http_server~encode_base64.
  ENDMETHOD.
  METHOD if_http_server~escape_html.
  ENDMETHOD.
  METHOD if_http_server~escape_url.
  ENDMETHOD.
  METHOD if_http_server~get_extension_info.
  ENDMETHOD.
  METHOD if_http_server~get_extension_url.
  ENDMETHOD.
  METHOD if_http_server~get_icf_runtime.
  ENDMETHOD.
  METHOD if_http_server~get_last_error.
  ENDMETHOD.
  METHOD if_http_server~get_location.
  ENDMETHOD.
  METHOD if_http_server~get_location_exception.
  ENDMETHOD.
  METHOD if_http_server~get_ucon_runtime.
  ENDMETHOD.
  METHOD if_http_server~get_xsrf_token.
  ENDMETHOD.
  METHOD if_http_server~is_xsrf_token_required.
  ENDMETHOD.
  METHOD if_http_server~logoff.
  ENDMETHOD.
  METHOD if_http_server~send_page.
  ENDMETHOD.
  METHOD if_http_server~set_compression.
  ENDMETHOD.
  METHOD if_http_server~set_page.
  ENDMETHOD.
  METHOD if_http_server~set_session_stateful.
  ENDMETHOD.
  METHOD if_http_server~set_session_stateful_via_url.
  ENDMETHOD.
  METHOD if_http_server~unescape_url.
  ENDMETHOD.
  METHOD if_http_server~validate_xsrf_token.
  ENDMETHOD.

ENDCLASS.

CLASS ltcl_handler DEFINITION FOR TESTING
  RISK LEVEL HARMLESS
  DURATION SHORT.
  PRIVATE SECTION.
    METHODS unknown_path_is_404   FOR TESTING.
    METHODS wrong_method_is_405   FOR TESTING.
    METHODS system_route_is_wired FOR TESTING.
ENDCLASS.

CLASS ltcl_handler IMPLEMENTATION.

  METHOD unknown_path_is_404.
    DATA(lo_srv) = NEW ltcl_server( iv_path = '/nope' iv_method = 'POST' ).
    NEW /xyz/cl_fooapi_handler( )->if_http_extension~handle_request( lo_srv ).
    cl_abap_unit_assert=>assert_equals( act = lo_srv->status( ) exp = 404 ).
    cl_abap_unit_assert=>assert_equals( act = lo_srv->body( )
                                        exp = '{"code":"path_not_found"}' ).
  ENDMETHOD.

  METHOD wrong_method_is_405.
    DATA(lo_srv) = NEW ltcl_server( iv_path = '/things' iv_method = 'GET' ).
    NEW /xyz/cl_fooapi_handler( )->if_http_extension~handle_request( lo_srv ).
    cl_abap_unit_assert=>assert_equals( act = lo_srv->status( ) exp = 405 ).
    cl_abap_unit_assert=>assert_equals(
      act = lo_srv->if_http_server~response->get_header_field( 'Allow' )
      exp = 'POST' ).
  ENDMETHOD.

  METHOD system_route_is_wired.
    " Catches a path that is in allowed_methods( ) but not in the CASE:
    " that answers 404 from WHEN OTHERS, not 200.
    DATA(lo_srv) = NEW ltcl_server( iv_path = '/system' iv_method = 'GET' ).
    NEW /xyz/cl_fooapi_handler( )->if_http_extension~handle_request( lo_srv ).
    cl_abap_unit_assert=>assert_equals(
      act = lo_srv->status( )
      exp = 200
      msg = |ACTUAL=>{ lo_srv->body( ) }<=ACTUAL| ).
    cl_abap_unit_assert=>assert_equals(
      act = lo_srv->if_http_server~response->get_content_type( )
      exp = 'application/json; charset=utf-8' ).
  ENDMETHOD.

ENDCLASS.
```

Write one "route is dispatched" test per path. It is the only test that sees a path missing from the dispatch `CASE`. Two limits of the double: it cannot tell `set_data( )` from `set_cdata( )` on ASCII bodies (you need an umlaut in the fixture for that), and it never sees ICF's own 401/403, which happen before the handler.

### `abaplint.json`

abaplint runs only the rules listed. JSON has no comments, so reasons go into `_comment_*` keys.

```json
{
  "global": {
    "files": "/src/**/*.*"
  },
  "syntax": {
    "version": "v740sp08",
    "errorNamespace": "^(Z|Y|/).*$"
  },
  "rules": {
    "parser_error": true,
    "method_implemented_twice": true,
    "unreachable_code": true,
    "unused_variables": true,
    "obsolete_statement": true,
    "when_others_last": true,
    "sql_escape_host_variables": true,
    "identical_form_names": true,
    "line_length": {
      "length": 160
    },
    "forbidden_identifier": {
      "check": ["^[/A-Za-z<][/A-Za-z0-9_<>]{30,}$"]
    }
  },
  "_comment_keyword_case": [
    "keyword_case is deliberately NOT enabled. SAP's Class Builder",
    "regenerates the DEFINITION section of every global class in its own",
    "style (lowercase keywords) on activation."
  ],
  "_comment_forbidden_identifier": [
    "The 30-character identifier cap, as a regex: abaplint has no rule",
    "for it, and a longer name is a hard compile error on SAP."
  ]
}
```

If you knowingly use a construct the 7.40 grammar rejects but your kernels accept, exclude that one file from `parser_error` (`"parser_error": {"exclude": ["<file>.clas.abap$"]}`) with a `_comment_` saying why. Add one entry per file, not a pattern, so every new exclusion is a deliberate act. Because the exclusion blinds the linter to every other parser error in that file, syntax-check the file on the oldest real kernel instead.

### Fixture table and setup report

Tests that need real rows read a small fixture table of your own, never business data. The table holds exactly the rows one report puts there, identically on every system. Two tests on two systems then compare like with like.

The table, `/XYZ/FOOAPI_FX`: client-dependent, transparent, key `MANDT` + `KEYCHAR` (`CHAR 20`), and one field per data type your code formats (`VAL_FLTP`, `VAL_DEC` as `DEC 15,2`, `VAL_DATS`, `VAL_STRG`, …). Table names are limited to 16 characters, and `/XYZ/` already uses 5 of them. Create it in SE11 or through ADT, then let abapGit serialize it. Choose key values where collations disagree if they disagree at all: case pairs, an embedded space, a hyphen, an underscore, a non-ASCII letter, digits with and without a space. An all-uppercase ASCII key set proves nothing about ordering.

```abap
REPORT /xyz/fooapi_fixture_setup.

" Fills /XYZ/FOOAPI_FX with a fixed row set. Idempotent: deletes first.
" A REPORT, not a classrun, so it compiles on every release.

DATA lt_rows   TYPE STANDARD TABLE OF /xyz/fooapi_fx WITH EMPTY KEY.
DATA lv_umlaut TYPE c LENGTH 1.
DATA lv_count  TYPE i.
DATA lv_msg    TYPE string.

START-OF-SELECTION.

  " Non-ASCII by code point, not as a source literal: the source passes
  " through git and abapGit, and a literal would make the stored bytes
  " depend on how each step handled the encoding.
  lv_umlaut = cl_abap_conv_in_ce=>uccp( uccp = '00C4' ).

  " MANDT is not set: Open SQL fills the client column on INSERT.
  " VAL_STRG uses backtick literals: a '...' literal would lose its
  " trailing blanks on the way into a string.
  lt_rows = VALUE #(
    ( keychar = 'ABC'     val_fltp = '0.1'     val_dec = '-1234567890.12'
      val_dats = '20240229' val_strg = `plain` )
    ( keychar = 'abc'     val_fltp = '1.0E+25' val_dec = '0.01'
      val_dats = '99991231' val_strg = `trailing space ` )
    ( keychar = 'A C'     val_fltp = '-2.5E-10' val_dec = '-0.01'
      val_dats = '00000000' val_strg = `` )
    ( keychar = 'A-C'     val_fltp = '0'       val_dec = '0'
      val_dats = '20000101' val_strg = `x` )
    ( keychar = 'A_C'     val_fltp = '3.14159265358979' val_dec = '99999999999.99'
      val_dats = '19000101' val_strg = ` leading and trailing ` )
    ( keychar = lv_umlaut val_fltp = '-0.1'    val_dec = '-99999999999.99'
      val_dats = '20241231' val_strg = `umlaut key` )
    ( keychar = '123'     val_fltp = '2.0'     val_dec = '123.45'
      val_dats = '20240101' val_strg = `123` )
    ( keychar = '1 3'     val_fltp = '1.0E-30' val_dec = '1.00'
      val_dats = '20240102' val_strg = `space in key` ) ).

  DELETE FROM /xyz/fooapi_fx.
  INSERT /xyz/fooapi_fx FROM TABLE @lt_rows.

  " Without ACCEPTING DUPLICATE KEYS this INSERT either succeeds or
  " dumps; the row count below is the real post-condition. Both
  " statements are in one LUW, so nothing is left half-filled.
  IF sy-subrc <> 0.
    ROLLBACK WORK.
    lv_msg = |FIXTURE_SETUP~FAILED~insert returned { sy-subrc }|.
    WRITE / lv_msg.
    RETURN.
  ENDIF.

  COMMIT WORK AND WAIT.

  " Also catches rows added by hand, which would silently break a
  " comparison between systems.
  SELECT COUNT( * ) FROM /xyz/fooapi_fx INTO @lv_count.
  IF lv_count <> lines( lt_rows ).
    lv_msg = |FIXTURE_SETUP~FAILED~expected { lines( lt_rows ) }~found { lv_count }|.
    WRITE / lv_msg.
    RETURN.
  ENDIF.

  " One string, one WRITE: separate operands would be padded into
  " output fields and break the ~ delimiters a script greps for.
  lv_msg = |FIXTURE_SETUP~OK~{ lv_count }~rows|.
  WRITE / lv_msg.
```

Run the report on each system after every change to the table or the rows. Unit tests may read the fixture under `RISK LEVEL HARMLESS` but never write to it. Every other test stays free of database access, and seams such as the presence interface above keep it that way. Keep `FLTP` literals well inside what every kernel can store. On the SAP_BASIS 750 system, literals with a magnitude below about `1.0E-64` (for example `1.0E-300`) were stored as zero, while the S/4HANA system stored them correctly. A value only one system can store makes a cross-system test fail because of how the row was inserted, not because of how your code formats it.

## 4. Data-heavy endpoints (considerations)

**The ABAP design for a data-heavy endpoint is not decided anywhere in this repo, and there is no ABAP template to point to.** This section is a list of things to weigh when you design one, not a specification, and none of the reference code in section 3 is meant as a starting point for it. The Go-side half of this topic — the on-premise response cap, streaming vs. buffering, timeouts, compression, payload shape and paging — is in [`docs/data-heavy-apis.md`](data-heavy-apis.md); the two documents are cross-linked because they're read by different audiences (the Go handler author vs. whoever writes the ABAP side) and deliberately don't repeat each other's content.

- **Expect each page to have to fit in memory: the writer holds the whole document before it hands any of it back.** The [JSON writer (sXML)](#json-writer-sxml) above builds the whole document in `cl_sxml_string_writer` before `get_output` hands the caller the finished bytes — there's no point in that pipeline where a partially-written document is emitted incrementally. Whatever paging scheme you pick, each page's size is bounded by what that writer can hold in the work process's memory at once, not by anything downstream.

  Expect nothing to reach Go until the page is finished: a classic ICF handler hands its response over when `handle_request` returns, so time-to-first-byte is the whole page's `SELECT` plus formatting time. That has to fit inside the tightest timeout in front of it (30 s at the approuter by default, see [the Go-side guide](data-heavy-apis.md#timeouts-across-the-chain)).

- **Expect a page's processing to have to complete within the work process's maximum runtime** — the same `rdisp/max_wprun_time` / `rdisp/scheduler/prio_*/max_runtime` limit already discussed above for the `TIME_OUT` dump. That bounds how much a single page can reasonably `SELECT` and format before the system kills the work process, independent of whatever deadline the Go side is using. See [§2 Runtime](#runtime) above: a page that exceeds this runtime or exhausts memory dumps (`TIME_OUT`, memory exhaustion) and answers HTML instead of JSON, which Go reports as a 502 `upstream_unreachable`, not as "page too big" — there is no distinguishable error for a page that was simply too large.

- **Expect keyset paging on the time key to be the natural fit for time-series-shaped data**: a `SELECT` filtered and ordered on the timestamp/sequence column, with the page boundary carried forward as a cursor rather than recomputed from a row count, on a unique key — if the timestamp alone isn't unique, include the series key. That's a statement about the shape of the data, not a prescription that it's the only workable option.

- **Format has to agree with the Go-side contract.** Whatever the ABAP side emits — timestamp format, how decimals are rendered — has to match what [`docs/data-heavy-apis.md`](data-heavy-apis.md) documents for the Go side. This is a two-way constraint: the Go side can't unilaterally fix a format the ABAP side doesn't also produce, and vice versa. §3's [`write_number`](#json-writer-sxml) takes `TYPE i` only; passing a decimal to it would drop the fractional part, so the endpoint has to format decimals explicitly and emit them with `write_string`, which matches the Go-side decimals-as-strings advice.
