// danger.js — typed confirmation for destructive actions (island spec §4.4).
//
// Markup contract (templates):
//   <form data-danger-confirm="owner/name">          (or data-danger-expected)
//     <input name="owner" type="text">               (any earlier text input)
//     <input data-danger-arm type="text">            ← the ONE that must match
//     <button data-danger-submit disabled>Delete</button>
//   </form>
//
// The armed input is marked explicitly (`data-danger-arm`, or the older
// `data-danger-input`) and only then falls back to the form's first text input.
// The marker is load-bearing on a two-field form: the transfer card has an
// owner field BEFORE the confirmation, and the old first-text-input fallback
// armed against the owner name, so the submit never enabled.
//
// The submit stays disabled until the typed value matches exactly; the server
// re-validates regardless (a disabled button is not a security boundary).

export function initDanger(root = document) {
  for (const form of root.querySelectorAll("[data-danger-confirm], [data-danger-expected]")) {
    if (form.dataset.dangerReady) continue;
    form.dataset.dangerReady = "1";
    const expected =
      form.getAttribute("data-danger-confirm") || form.getAttribute("data-danger-expected") || "";
    const input =
      form.querySelector("[data-danger-arm]") ||
      form.querySelector("[data-danger-input]") ||
      form.querySelector("input[type=text]");
    const submit = form.querySelector("[data-danger-submit], button[type=submit]");
    if (!input || !submit) continue;
    const arm = () => {
      const ok = input.value.trim() === expected;
      submit.disabled = !ok;
      submit.setAttribute("aria-disabled", String(!ok));
      input.setAttribute("aria-invalid", String(!ok && input.value !== ""));
    };
    input.addEventListener("input", arm);
    arm();
  }
}
