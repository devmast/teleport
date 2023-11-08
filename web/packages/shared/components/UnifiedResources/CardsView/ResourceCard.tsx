/**
 * Copyright 2023 Gravitational, Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import React, { useState, useEffect, useLayoutEffect, useRef } from 'react';
import styled, { css } from 'styled-components';

import { Box, ButtonLink, Flex, Label, Text } from 'design';
import { StyledCheckbox } from 'design/Checkbox';

import { ShimmerBox } from 'design/ShimmerBox';
import { ResourceIcon } from 'design/ResourceIcon';

import { HoverTooltip } from '../UnifiedResources';

import { ResourceItemProps } from '../types';
import { PinButton, CopyButton } from '../shared';

// Since we do a lot of manual resizing and some absolute positioning, we have
// to put some layout constants in place here.
const labelHeight = 20; // px
const labelVerticalMargin = 1; // px
const labelRowHeight = labelHeight + labelVerticalMargin * 2;

/**
 * This box serves twofold purpose: first, it prevents the underlying icon from
 * being squeezed if the parent flexbox starts shrinking items. Second, it
 * prevents the icon from magically occupying too much space, since the SVG
 * element somehow forces the parent to occupy at least full line height.
 */
const ResTypeIconBox = styled(Box)`
  line-height: 0;
`;

export function ResourceCard({
  name,
  primaryIconName,
  SecondaryIcon,
  onLabelClick,
  addr,
  type,
  ActionButton,
  labels,
  pinningSupport,
  pinned,
  pinResource,
  selectResource,
  selected,
}: ResourceItemProps) {
  const [showMoreLabelsButton, setShowMoreLabelsButton] = useState(false);
  const [showAllLabels, setShowAllLabels] = useState(false);
  const [numMoreLabels, setNumMoreLabels] = useState(0);
  const [isNameOverflowed, setIsNameOverflowed] = useState(false);

  const [hovered, setHovered] = useState(false);

  const innerContainer = useRef<Element | null>(null);
  const labelsInnerContainer = useRef(null);
  const nameText = useRef<HTMLDivElement | null>(null);
  const collapseTimeout = useRef<ReturnType<typeof setTimeout>>(null);

  // This effect installs a resize observer whose purpose is to detect the size
  // of the component that contains all the labels. If this component is taller
  // than the height of a single label row, we show a "+x more" button.
  useLayoutEffect(() => {
    if (!labelsInnerContainer.current) return;

    const observer = new ResizeObserver(entries => {
      // This check will let us know if the name text has overflowed. We do this
      // to conditionally render a tooltip for only overflowed names
      if (
        nameText.current?.scrollWidth >
        nameText.current?.parentElement.offsetWidth
      ) {
        setIsNameOverflowed(true);
      } else {
        setIsNameOverflowed(false);
      }
      const container = entries[0];

      // We're taking labelRowHeight * 1.5 just in case some glitch adds or
      // removes a pixel here and there.
      const moreThanOneRow =
        container.contentBoxSize[0].blockSize > labelRowHeight * 1.5;
      setShowMoreLabelsButton(moreThanOneRow);

      // Count number of labels in the first row. This will let us calculate and
      // show the number of labels left out from the view.
      const labelElements = [
        ...entries[0].target.querySelectorAll('[data-is-label]'),
      ];
      const firstLabelPos = labelElements[0]?.getBoundingClientRect().top;
      // Find the first one below.
      const firstLabelInSecondRow = labelElements.findIndex(
        e => e.getBoundingClientRect().top > firstLabelPos
      );

      setNumMoreLabels(
        firstLabelInSecondRow > 0
          ? labelElements.length - firstLabelInSecondRow
          : 0
      );
    });

    observer.observe(labelsInnerContainer.current);
    return () => {
      observer.disconnect();
    };
  });

  // Clear the timeout on unmount to prevent changing a state of an unmounted
  // component.
  useEffect(() => () => clearTimeout(collapseTimeout.current), []);

  const onMoreLabelsClick = () => {
    setShowAllLabels(true);
  };

  const onMouseLeave = () => {
    // If the user expanded the labels and then scrolled down enough to hide the
    // top of the card, we scroll back up and collapse the labels with a small
    // delay to keep the user from losing focus on the card that they were
    // looking at. The delay is picked by hand, since there's no (easy) way to
    // know when the animation ends.
    if (
      showAllLabels &&
      (innerContainer.current?.getBoundingClientRect().top ?? 0) < 0
    ) {
      innerContainer.current?.scrollIntoView({
        behavior: 'smooth',
        block: 'start',
      });
      clearTimeout(collapseTimeout.current);
      collapseTimeout.current = setTimeout(() => setShowAllLabels(false), 700);
    } else {
      // Otherwise, we just collapse the labels immediately.
      setShowAllLabels(false);
    }
  };

  return (
    <CardContainer
      onMouseEnter={() => setHovered(true)}
      onMouseLeave={() => setHovered(false)}
    >
      <CardOuterContainer showAllLabels={showAllLabels}>
        <CardInnerContainer
          ref={innerContainer}
          p={3}
          // we set padding left a bit larger so we can have space to absolutely
          // position the pin/checkbox buttons
          pl={6}
          alignItems="start"
          onMouseLeave={onMouseLeave}
          pinned={pinned}
          selected={selected}
        >
          <HoverTooltip tipContent={<>{selected ? 'Deselect' : 'Select'}</>}>
            <StyledCheckbox
              css={`
                position: absolute;
                top: 16px;
                left: 16px;
              `}
              checked={selected}
              onChange={selectResource}
            />
          </HoverTooltip>
          <Box
            css={`
              position: absolute;
              // we position far from the top so the layout of the pin doesn't change if we expand the card
              top: ${props => props.theme.space[9]}px;
              transition: none;
              left: 16px;
            `}
          >
            <PinButton
              setPinned={pinResource}
              pinned={pinned}
              pinningSupport={pinningSupport}
              hovered={hovered}
            />
          </Box>
          <ResourceIcon
            name={primaryIconName}
            width="45px"
            height="45px"
            ml={2}
          />
          {/* MinWidth is important to prevent descriptions from overflowing. */}
          <Flex flexDirection="column" flex="1" minWidth="0" ml={3} gap={1}>
            <Flex flexDirection="row" alignItems="center">
              <SingleLineBox flex="1">
                {isNameOverflowed ? (
                  <HoverTooltip tipContent={<>{name}</>}>
                    <Text ref={nameText} typography="h5" fontWeight={300}>
                      {name}
                    </Text>
                  </HoverTooltip>
                ) : (
                  <Text ref={nameText} typography="h5" fontWeight={300}>
                    {name}
                  </Text>
                )}
              </SingleLineBox>
              {hovered && <CopyButton name={name} />}
              {ActionButton}
            </Flex>
            <Flex flexDirection="row" alignItems="center">
              <ResTypeIconBox>
                <SecondaryIcon size={18} />
              </ResTypeIconBox>
              {type && (
                <SingleLineBox ml={1} title={type}>
                  <Text typography="body2" color="text.slightlyMuted">
                    {type}
                  </Text>
                </SingleLineBox>
              )}
              {addr && (
                <SingleLineBox ml={2} title={addr}>
                  <Text typography="body2" color="text.muted">
                    {addr}
                  </Text>
                </SingleLineBox>
              )}
            </Flex>
            <LabelsContainer showAll={showAllLabels}>
              <LabelsInnerContainer ref={labelsInnerContainer}>
                <MoreLabelsButton
                  style={{
                    visibility:
                      showMoreLabelsButton && !showAllLabels
                        ? 'visible'
                        : 'hidden',
                  }}
                  onClick={onMoreLabelsClick}
                >
                  + {numMoreLabels} more
                </MoreLabelsButton>
                {labels.map((label, i) => {
                  const { name, value } = label;
                  const labelText = `${name}: ${value}`;
                  return (
                    <StyledLabel
                      key={JSON.stringify([name, value, i])}
                      title={labelText}
                      onClick={() => onLabelClick?.(label)}
                      kind="secondary"
                      data-is-label=""
                    >
                      {labelText}
                    </StyledLabel>
                  );
                })}
              </LabelsInnerContainer>
            </LabelsContainer>
          </Flex>
        </CardInnerContainer>
      </CardOuterContainer>
    </CardContainer>
  );
}

type LoadingCardProps = {
  delay?: 'none' | 'short' | 'long';
};

const DelayValueMap = {
  none: 0,
  short: 400, // 0.4s;
  long: 600, // 0.6s;
};

function randomNum(min: number, max: number) {
  return Math.floor(Math.random() * (max - min + 1)) + min;
}

export function LoadingCard({ delay = 'none' }: LoadingCardProps) {
  const [canDisplay, setCanDisplay] = useState(false);

  useEffect(() => {
    const displayTimeout = setTimeout(() => {
      setCanDisplay(true);
    }, DelayValueMap[delay]);
    return () => {
      clearTimeout(displayTimeout);
    };
  }, []);

  if (!canDisplay) {
    return null;
  }

  return (
    <LoadingCardWrapper p={3}>
      <Flex gap={2} alignItems="start">
        {/* Image */}
        <ShimmerBox height="45px" width="45px" />
        {/* Name and action button */}
        <Box flex={1}>
          <Flex gap={2} mb={2} justifyContent="space-between">
            <ShimmerBox
              height="24px"
              css={`
                flex-basis: ${randomNum(100, 30)}%;
              `}
            />
            <ShimmerBox height="24px" width="90px" />
          </Flex>
          <ShimmerBox height="16px" width={`${randomNum(90, 40)}%`} mb={2} />
          <Box>
            <Flex gap={2}>
              {new Array(randomNum(4, 0)).fill(null).map((_, i) => (
                <ShimmerBox key={i} height="16px" width="60px" />
              ))}
            </Flex>
          </Box>
        </Box>
      </Flex>
    </LoadingCardWrapper>
  );
}

/**
 * The outer container's purpose is to reserve horizontal space on the resource
 * grid. It holds the inner container that normally holds a regular layout of
 * the card, and is fully contained inside the outer container.  Once the user
 * clicks the "more" button, the inner container "pops out" by changing its
 * position to absolute.
 *
 * TODO(bl-nero): Known issue: this doesn't really work well with one-column
 * layout; we may need to globally set the card height to fixed size on the
 * outer container.
 */
const CardContainer = styled(Box)`
  position: relative;
`;

const CardOuterContainer = styled(Box)`
  border-radius: ${props => props.theme.radii[3]}px;

  ${props =>
    props.showAllLabels &&
    css`
      position: absolute;
      left: 0;
      right: 0;
      z-index: 1;
    `}
  transition: all 150ms;

  ${CardContainer}:hover & {
    background-color: ${props => props.theme.colors.levels.surface};

    // We use a pseudo element for the shadow with position: absolute in order to prevent
    // the shadow from increasing the size of the layout and causing scrollbar flicker.
    :after {
      box-shadow: ${props => props.theme.boxShadow[3]};
      border-radius: ${props => props.theme.radii[3]}px;
      content: '';
      position: absolute;
      top: 0;
      left: 0;
      z-index: -1;
      width: 100%;
      height: 100%;
    }
  }
`;

/**
 * The inner container that normally holds a regular layout of the card, and is
 * fully contained inside the outer container.  Once the user clicks the "more"
 * button, the inner container "pops out" by changing its position to absolute.
 *
 * TODO(bl-nero): Known issue: this doesn't really work well with one-column
 * layout; we may need to globally set the card height to fixed size on the
 * outer container.
 */
const CardInnerContainer = styled(Flex)`
  border: ${props => props.theme.borders[2]}
    ${props => props.theme.colors.spotBackground[0]};
  border-radius: ${props => props.theme.radii[3]}px;
  background-color: ${props => getBackgroundColor(props)};

  :hover {
    // Make the border invisible instead of removing it, this is to prevent things from shifting due to the size change.
    border: ${props => props.theme.borders[2]} rgba(0, 0, 0, 0);
  }
`;

const getBackgroundColor = props => {
  if (props.selected) {
    return props.theme.colors.interactive.tonal.primary[2];
  }
  if (props.pinned) {
    return props.theme.colors.interactive.tonal.primary[0];
  }
  return 'transparent';
};

const SingleLineBox = styled(Box)`
  overflow: hidden;
  white-space: nowrap;
  text-overflow: ellipsis;
`;

/**
 * The outer labels container is resized depending on whether we want to show a
 * single row, or all labels. It hides the internal container's overflow if more
 * than one row of labels exist, but is not yet visible.
 */
const LabelsContainer = styled(Box)`
  ${props => (props.showAll ? '' : `height: ${labelRowHeight}px;`)}
  overflow: hidden;
`;

const StyledLabel = styled(Label)`
  height: ${labelHeight}px;
  margin: 1px 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  cursor: pointer;
  line-height: ${labelHeight - labelVerticalMargin}px;
`;

/**
 * The inner labels container always adapts to the size of labels.  Its height
 * is measured by the resize observer.
 */
const LabelsInnerContainer = styled(Flex)`
  position: relative;
  flex-wrap: wrap;
  align-items: start;
  gap: ${props => props.theme.space[1]}px;
  padding-right: 60px;
`;

/**
 * It's important for this button to use absolute positioning; otherwise, its
 * presence in the layout may itself influence the resize logic, potentially
 * causing a feedback loop.
 */
const MoreLabelsButton = styled(ButtonLink)`
  position: absolute;
  right: 0;

  height: ${labelHeight}px;
  margin: ${labelVerticalMargin}px 0;
  min-height: 0;

  background-color: ${props => getBackgroundColor(props)};
  color: ${props => props.theme.colors.text.slightlyMuted};
  font-style: italic;

  transition: visibility 0s;
  transition: background 150ms;
`;

const LoadingCardWrapper = styled(Box)`
  height: 100px;
  border: ${props => props.theme.borders[2]}
    ${props => props.theme.colors.spotBackground[0]};
  border-radius: ${props => props.theme.radii[3]}px;
`;
