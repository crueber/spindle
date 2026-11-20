// danger.js — typed confirmation for destructive actions (island spec §4.4).
//
// Markup contract (templates):
//   <form data-danger-confirm="owner/name">          (or data-danger-expected)
//     <input data-danger-input …>
//     <button data-danger-submit disabled>Delete</button>
//   </form>
//
// The submit stays disabled until the typed value matches exactly; the server
// re-validates regardless (a disabled button is not a security boundary).

export function initDanger(root = document) {
  for (const form of root.querySelectorAll("[data-danger-confirm], [data-danger-expected]")) {
    if (form.dataset.dangerReady) continue;
    form.dataset.dangerReady = "1";
    const expected =
      form.getAttribute("data-danger-confirm") || form.getAttribute("data-danger-expected") || "";
    const input = form.querySelector("[data-danger-input]") || form.querySelector("input[type=text]");
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
