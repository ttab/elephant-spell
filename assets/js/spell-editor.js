// spell-editor.js — progressive enhancement for the dictionary entry form.
//
// It registers a small code-input template that highlights the {A|B} expansion
// syntax used in common mistakes (and, later, rule patterns), wires the
// add/remove buttons for the structured "forms" rows, and shows a live,
// server-validated preview of how each common-mistakes pattern expands.
//
// All behaviour is attached through event delegation on the document so it
// keeps working for form markup swapped in by htmx.
(function () {
  "use strict";

  // --- syntax highlighting -------------------------------------------------

  // highlightPattern turns the expansion syntax into coloured spans. The input
  // is plain text; we escape it first and then wrap the structural characters.
  function highlightPattern(text) {
    var escaped = text
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;");

    return escaped
      .replace(/[{}]/g, function (m) {
        return '<span class="tok-brace">' + m + "</span>";
      })
      .replace(/\|/g, '<span class="tok-pipe">|</span>');
  }

  if (window.codeInput) {
    codeInput.registerTemplate("spell-pattern", {
      highlight: function (resultElement, codeInputElement) {
        resultElement.innerHTML = highlightPattern(codeInputElement.value);
      },
      includeCodeInputInHighlightFunc: true,
      preElementStyled: true,
      isCode: false,
      plugins: [],
    });
  }

  // --- forms rows ----------------------------------------------------------

  document.addEventListener("click", function (e) {
    var add = e.target.closest("[data-add-form-row]");
    if (add) {
      e.preventDefault();

      var tpl = document.getElementById("forms-row-template");
      var rows = document.getElementById("forms-rows");
      if (tpl && rows) {
        rows.appendChild(tpl.content.cloneNode(true));
        var inputs = rows.querySelectorAll(".forms-row:last-child input");
        if (inputs.length) {
          inputs[0].focus();
        }
      }
      return;
    }

    var remove = e.target.closest("[data-row-remove]");
    if (remove) {
      e.preventDefault();
      var row = remove.closest(".forms-row");
      if (row) {
        row.remove();
      }
    }
  });

  // --- common-mistakes validation preview ----------------------------------

  var timers = new WeakMap();

  function scheduleValidate(el) {
    clearTimeout(timers.get(el));
    timers.set(
      el,
      setTimeout(function () {
        validate(el);
      }, 400)
    );
  }

  function validate(el) {
    var url = el.getAttribute("data-validate-url");
    var targetSel = el.getAttribute("data-preview-target");
    if (!url || !targetSel) {
      return;
    }

    var target = document.querySelector(targetSel);
    if (!target) {
      return;
    }

    var body = new URLSearchParams();
    body.set("common_mistakes", el.value);

    fetch(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
        "HX-Request": "true",
      },
      body: body.toString(),
      credentials: "same-origin",
    })
      .then(function (r) {
        return r.ok ? r.text() : Promise.reject(r.status);
      })
      .then(function (html) {
        target.innerHTML = html;
      })
      .catch(function () {
        /* leave the last preview in place on transient errors */
      });
  }

  document.addEventListener("input", function (e) {
    var el =
      e.target.closest && e.target.closest("code-input[data-validate-url]");
    if (el) {
      scheduleValidate(el);
    }
  });
})();
