// The parts of the page that VitePress's theme leaves to the browser, set here once the page is up: the sidebar shows the current page,
// the group titles are not controls, a table is a stop for the Tab key only if it scrolls, the phone menu marks the section, and the
// search gives the keyboard back to where it came from.

import { useData, useRoute } from "vitepress";
import { nextTick, onBeforeUnmount, onMounted, watch } from "vue";

/** The path of the current page as the nav's `activeMatch` reads it: `/runbooks/upgrade`. */
function currentPath(path: string): string {
    return decodeURI(path).replace(/[?#].*$/, "").replace(/(?:(^|\/)index)?(?:\.html)?$/, "$1") || "/";
}

export function useChrome(): void {
    const route = useRoute();
    const { theme } = useData();

    /** The group titles of the sidebar have nothing to open, so they are not controls: no role, no stop for Tab. */
    function settleGroupTitles(): void {
        for (const title of document.querySelectorAll<HTMLElement>(".VPSidebarItem.level-0:not(.collapsible) > .item[role='button']")) {
            title.removeAttribute("role");
            title.removeAttribute("tabindex");
        }
    }

    /** The sidebar opens with the current page in view, a third of the way down, when it would be out of sight. */
    function showCurrentPage(): void {
        const sidebar = document.querySelector<HTMLElement>(".VPSidebar");
        const item = sidebar?.querySelector<HTMLElement>(".VPSidebarItem.is-active > .item");
        if (!sidebar || !item) return;

        const frame = sidebar.getBoundingClientRect();
        const box = item.getBoundingClientRect();
        if (box.top < frame.top + 96 || box.bottom > frame.bottom - 48) sidebar.scrollTop += box.top - frame.top - frame.height / 3;
    }

    /** A table that scrolls sideways is a stop for Tab, so the keyboard can scroll it. One that fits is not. A block of code is the same. A table that would scroll with its words of code whole is `tight`: they may break. */
    function settleTables(): void {
        for (const wrap of document.querySelectorAll<HTMLElement>(".vp-doc .table-wrap")) {
            wrap.classList.remove("tight");
            if (wrap.scrollWidth > wrap.clientWidth + 1) wrap.classList.add("tight");
            if (wrap.scrollWidth > wrap.clientWidth + 1) {
                wrap.setAttribute("tabindex", "0");
                wrap.setAttribute("role", "region");
                wrap.setAttribute("aria-label", "Table, scrolls sideways");
            } else {
                wrap.removeAttribute("tabindex");
                wrap.removeAttribute("role");
                wrap.removeAttribute("aria-label");
            }
        }

        for (const code of document.querySelectorAll<HTMLElement>(".vp-doc div[class*='language-'] > pre")) {
            if (code.scrollWidth > code.clientWidth + 1) code.setAttribute("tabindex", "0");
            else code.removeAttribute("tabindex");
        }
    }

    /** A link in a page that leaves the site opens a new tab, as the mark beside it says: the few made by a template are set here. */
    function settleLinks(): void {
        for (const link of document.querySelectorAll<HTMLAnchorElement>(".vp-doc a[href^='http']:not([target])")) {
            if (new URL(link.href).origin === location.origin) continue;
            link.setAttribute("target", "_blank");
            link.setAttribute("rel", "noreferrer");
        }
    }

    /** The outline beside a page keeps its current entry in view when it is longer than the screen, without moving the page. */
    function showCurrentHeading(): void {
        const link = document.querySelector<HTMLElement>(".VPDocAsideOutline .outline-link.active");
        if (!link) return;

        let frame: HTMLElement | null = link.parentElement;
        while (frame && !(frame.scrollHeight > frame.clientHeight && /auto|scroll/.test(getComputedStyle(frame).overflowY))) frame = frame.parentElement;
        if (!frame) return;

        // The aside runs up under the masthead: its visible part starts below it.
        const view = frame.getBoundingClientRect();
        const top = Math.max(view.top, document.querySelector(".VPNav")?.getBoundingClientRect().bottom ?? 0) + 16;
        const box = link.getBoundingClientRect();
        if (box.top < top) frame.scrollTop += box.top - top;
        else if (box.bottom > view.bottom - 16) frame.scrollTop += box.bottom - view.bottom + 16;
    }

    /** The phone menu has no mark of its own for the section: it takes the nav's, by the same `activeMatch`. */
    function markMenu(): void {
        const path = currentPath(route.path);
        const items = (theme.value.nav ?? []).filter((item): item is { text: string; link: string; activeMatch?: string } => "link" in item);
        const current = items.find((item) => (item.activeMatch ? new RegExp(item.activeMatch).test(path) : currentPath(item.link) === path));

        for (const link of document.querySelectorAll<HTMLAnchorElement>(".VPNavScreenMenuLink")) {
            const active = current !== undefined && currentPath(new URL(link.href, location.href).pathname) === currentPath(current.link);
            if (link.classList.contains("active") !== active) link.classList.toggle("active", active);
            if (active) link.setAttribute("aria-current", "page");
            else link.removeAttribute("aria-current");
        }
    }

    function settle(): void {
        settleGroupTitles();
        showCurrentPage();
        settleTables();
        settleLinks();
        markMenu();
        watchOutline();
    }

    let frame = 0;
    function later(): void {
        cancelAnimationFrame(frame);
        frame = requestAnimationFrame(settleTables);
    }

    /** A heading reached by its address clears the bars. The faces load after the browser has scrolled, and move it up: set it again, unless the reader has moved on. */
    function settleHash(): void {
        if (!location.hash) return;

        let moved = false;
        for (const name of ["wheel", "touchstart", "keydown", "mousedown"]) window.addEventListener(name, () => (moved = true), { once: true, passive: true });

        void document.fonts?.ready.then(() =>
            requestAnimationFrame(() => {
                const target = moved ? null : document.getElementById(decodeURIComponent(location.hash.slice(1)));
                if (target && target.getBoundingClientRect().top < parseFloat(getComputedStyle(document.documentElement).scrollPaddingTop) - 2) target.scrollIntoView();
            })
        );
    }

    /** The outline marks its current entry by a class, which the theme's script moves as the page scrolls: look at it as it moves. */
    let outline: MutationObserver | undefined;
    function watchOutline(): void {
        outline?.disconnect();
        const aside = document.querySelector(".VPDocAsideOutline");
        if (!aside) return;
        outline = new MutationObserver(showCurrentHeading);
        outline.observe(aside, { attributes: true, attributeFilter: ["class"], subtree: true });
        showCurrentHeading();
    }

    let menu: MutationObserver | undefined;
    let search: MutationObserver | undefined;

    onMounted(() => {
        void nextTick(settle);
        settleHash();
        // Tables measure again when the faces are in, and when the window changes.
        void document.fonts?.ready.then(settleTables);
        document.fonts?.addEventListener("loadingdone", later);
        window.addEventListener("resize", later);

        // The menu is built when it opens: mark it as it appears.
        const nav = document.querySelector(".VPNav");
        if (nav) {
            menu = new MutationObserver(markMenu);
            menu.observe(nav, { childList: true, subtree: true });
        }

        // The search box is built on the page's body and removed on close. Closed, it hands the keyboard back to the search button
        // unless the keyboard is already somewhere else.
        search = new MutationObserver((records) => {
            const closed = records.some((record) => [...record.removedNodes].some((node) => node instanceof HTMLElement && node.classList.contains("VPLocalSearchBox")));
            if (!closed) return;
            requestAnimationFrame(() => {
                if (document.activeElement && document.activeElement !== document.body) return;
                document.querySelector<HTMLElement>("#local-search button")?.focus();
            });
        });
        search.observe(document.body, { childList: true });
    });

    watch(
        () => route.path,
        () => void nextTick(settle)
    );

    onBeforeUnmount(() => {
        menu?.disconnect();
        search?.disconnect();
        outline?.disconnect();
        window.removeEventListener("resize", later);
        document.fonts?.removeEventListener("loadingdone", later);
        cancelAnimationFrame(frame);
    });
}
