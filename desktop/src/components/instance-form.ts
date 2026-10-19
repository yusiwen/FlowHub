import type { Instance, InstanceInput, InstanceKind } from "../types";

const KINDS: ReadonlyArray<{ value: InstanceKind; label: string }> = [
  { value: "dsh-web", label: "dsh web" },
  { value: "opencode", label: "opencode" },
  { value: "other", label: "other" },
];

function fieldValue(form: HTMLFormElement, name: string): string {
  return (form.elements.namedItem(name) as HTMLInputElement | null)?.value ?? "";
}

export function openInstanceForm(
  existing?: Instance,
): Promise<InstanceInput | null> {
  return new Promise((resolve) => {
    let result: InstanceInput | null = null;

    const dialog = document.createElement("dialog");
    dialog.className = "dialog";
    dialog.innerHTML = `
      <form class="form">
        <h2>${existing === undefined ? "Add instance" : "Edit instance"}</h2>
        <label>Name
          <input name="name" autocomplete="off" placeholder="dsh 3080" />
        </label>
        <label>Kind
          <select name="kind">
            ${KINDS.map((kind) => `<option value="${kind.value}">${kind.label}</option>`).join("")}
          </select>
        </label>
        <label>URL
          <input name="url" autocomplete="off" placeholder="http://127.0.0.1:3080/" />
        </label>
        <label>Group <span class="optional">(optional)</span>
          <input name="group" autocomplete="off" placeholder="local" />
        </label>
        <label>Notes <span class="optional">(optional)</span>
          <textarea name="notes" rows="2"></textarea>
        </label>
        <p class="hint" data-hint></p>
        <p class="form-error" role="alert" hidden></p>
        <div class="dialog-actions">
          <button type="button" class="ghost-button" data-cancel>Cancel</button>
          <button type="submit" class="primary-button">Save</button>
        </div>
      </form>
    `;

    const form = dialog.querySelector("form") as HTMLFormElement;
    const nameInput = form.elements.namedItem("name") as HTMLInputElement;
    const kindSelect = form.elements.namedItem("kind") as HTMLSelectElement;
    const urlInput = form.elements.namedItem("url") as HTMLInputElement;
    const groupInput = form.elements.namedItem("group") as HTMLInputElement;
    const notesInput = form.elements.namedItem("notes") as HTMLTextAreaElement;
    const hint = dialog.querySelector("[data-hint]") as HTMLElement;
    const error = dialog.querySelector(".form-error") as HTMLElement;

    nameInput.value = existing?.name ?? "";
    kindSelect.value = existing?.kind ?? "dsh-web";
    urlInput.value = existing?.url ?? "";
    groupInput.value = existing?.group ?? "";
    notesInput.value = existing?.notes ?? "";

    const updateHint = (): void => {
      hint.textContent =
        kindSelect.value === "dsh-web"
          ? "dsh web signs in with the URL printed by the dsh web command (the launch token)."
          : "Loaded directly; no token is needed.";
    };
    updateHint();
    kindSelect.addEventListener("change", updateHint);

    const showError = (message: string): void => {
      error.textContent = message;
      error.hidden = false;
    };

    form.addEventListener("submit", (event) => {
      event.preventDefault();
      const name = nameInput.value.trim();
      if (name === "") {
        showError("Name is required.");
        nameInput.focus();
        return;
      }
      const rawUrl = urlInput.value.trim();
      let parsed: URL;
      try {
        parsed = new URL(rawUrl);
      } catch {
        showError("URL is not valid.");
        urlInput.focus();
        return;
      }
      if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
        showError("URL must use http or https.");
        urlInput.focus();
        return;
      }
      result = {
        name,
        kind: kindSelect.value as InstanceKind,
        url: rawUrl,
        group: groupInput.value.trim() === "" ? null : groupInput.value.trim(),
        notes: notesInput.value,
      };
      dialog.close();
    });

    (dialog.querySelector("[data-cancel]") as HTMLButtonElement).onclick = () => {
      dialog.close();
    };

    dialog.addEventListener("close", () => {
      dialog.remove();
      resolve(result);
    });

    document.body.append(dialog);
    dialog.showModal();
    nameInput.focus();
  });
}

export function confirmRemove(name: string): Promise<boolean> {
  return new Promise((resolve) => {
    const dialog = document.createElement("dialog");
    dialog.className = "dialog";
    dialog.innerHTML = `
      <form class="form">
        <h2>Delete instance</h2>
        <p class="dialog-text"></p>
        <div class="dialog-actions">
          <button type="button" class="ghost-button" data-cancel>Cancel</button>
          <button type="submit" class="danger-button">Delete</button>
        </div>
      </form>
    `;
    (dialog.querySelector(".dialog-text") as HTMLElement).textContent =
      `Remove “${name}” from the sidebar?`;

    const form = dialog.querySelector("form") as HTMLFormElement;
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      dialog.close("remove");
    });
    (dialog.querySelector("[data-cancel]") as HTMLButtonElement).onclick = () => {
      dialog.close("cancel");
    };
    dialog.addEventListener("close", () => {
      dialog.remove();
      resolve(dialog.returnValue === "remove");
    });

    document.body.append(dialog);
    dialog.showModal();
  });
}
